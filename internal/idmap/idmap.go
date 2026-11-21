// Package idmap 负责 QQ string openid 与 OneBot int64 id 之间的映射，
// 以及 message_id 的确定性哈希分配、被动回复所需的原始 msg_id 池。
// 可选用 bbolt 持久化（反向 openid 与 message_id 元信息），使重启后仍可解析。
package idmap

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Scene 消息场景
type Scene string

const (
	// SceneGroup 群聊
	SceneGroup Scene = "group"
	// SceneC2C 单聊
	SceneC2C Scene = "c2c"
)

// 被动回复有效期与每条消息可回复次数（官方规则）：
// 群聊、私聊的被动回复都是 60 分钟；群消息每条最多回复 5 次（机器人开发者是该群群主或管理员时 50 次），
// 私聊消息每条最多回复 4 次。群消息也可以私聊回复它的发送者，次数和群内回复共用
const (
	// GroupPassiveTTL 群被动回复有效期
	GroupPassiveTTL = 60 * time.Minute
	// C2CPassiveTTL 单聊被动回复有效期
	C2CPassiveTTL = 60 * time.Minute

	// GroupPassiveReplies 群消息每条可被动回复次数
	GroupPassiveReplies = 5
	// GroupPassiveRepliesBoost 机器人开发者是群主或管理员时，群消息每条可被动回复次数提升到 50
	GroupPassiveRepliesBoost = 50
	// C2CPassiveReplies 单聊消息每条可被动回复次数
	C2CPassiveReplies = 4
)

// 后台清理（janitor）参数：控制长期运行时内存和 idmap.db 的大小。
//   - msg2meta / lazy / seqCounter / joinReq：有时效，按时间和条数上限回收
//   - id2open / open2id：持久化模式下只是 db 的缓存，超过上限时淘汰一部分（用到时再从 db 读回）；
//     纯内存模式下它们是唯一的记录，不清理
//   - activeCount：每天的主动消息计数，过了当天就回收
//   - db 里的消息元信息按 Options.KeepDays 保留天数、Options.MaxBytes 文件大小上限清理，idmap.db 不再无限增长
//
// 除了定时清理，新增条目每到 sweepEvery 条也会提前清理一次，避免消息很多时在两次定时清理之间涨得太高。
const (
	// janitorInterval 后台清理扫描周期
	janitorInterval = 5 * time.Minute
	// sweepEvery 新增这么多条目后提前清理一次
	sweepEvery = 10000
	// msgMetaTTL 内存中消息元信息保留时长（超时后从内存淘汰；持久化模式下仍可回落 db 重载）
	msgMetaTTL = 2 * time.Hour
	// msgMetaCap 内存中消息元信息条数硬上限（超出按时间淘汰最旧）
	msgMetaCap = 30000
	// lazyCap 被动回复凭证条数硬上限
	lazyCap = 30000
	// seqTTL msg_seq 计数器空闲保留时长（被动窗口过后即可回收）
	seqTTL = C2CPassiveTTL
	// seqCap msg_seq 计数器条数硬上限
	seqCap = 30000
	// joinReqTTL 入群申请上下文兜底保留时长（正常审批后即删除，此为未处理时的回收阈值）
	joinReqTTL = 24 * time.Hour
	// idCacheCap 持久化模式下内存里最多缓存多少个 openid（超出时淘汰到 idCacheKeep，用到时从 db 读回）
	idCacheCap  = 30000
	idCacheKeep = 20000
	// dbPruneInterval 多久清理一次 db 里过期的消息元信息（以及检查文件大小上限）
	dbPruneInterval = time.Hour
	// freeOSAfter 一次清理回收的条目超过这个数时，把空出来的内存还给系统
	freeOSAfter = 20000
)

// seqEntry msg_seq 计数器条目，带最近使用时间以支持空闲回收
type seqEntry struct {
	n    int32
	last int64 // 最近使用时间（UnixNano），atomic 读写
}

// MsgMeta 消息元信息：入站供被动回复；出站供后续引用机器人自己发的消息
type MsgMeta struct {
	MsgID       string // QQ 原始消息 id（ROBOT1.0_...）；仅入站消息可用作被动回复窗口
	RefIdx      string // 引用索引令牌（REFIDX_...）。入站来自 message_scene.ext 的 msg_idx；出站来自发消息回包 ext_info.ref_idx
	GroupOpenID string
	UserOpenID  string
	Scene       Scene
	Time        time.Time
	Outbound    bool // true=本程序发出的消息，不能当作被动 msg_id
}

// PassiveOK 这条消息现在还能不能当被动回复凭证（用户发来的消息，且还在有效期内）
func (s *Store) PassiveOK(m MsgMeta) bool {
	if m.Outbound || m.MsgID == "" || m.Time.IsZero() {
		return false
	}
	ttl := C2CPassiveTTL
	if m.Scene != SceneC2C {
		ttl = GroupPassiveTTL
	}
	return time.Since(m.Time) < ttl
}

type lazyEntry struct {
	msgID string
	kind  string // "msg"=被动 msg_id，"event"=被动 event_id（互动事件）
	at    time.Time
}

// bbolt 桶名
var (
	bucketIDs      = []byte("ids")      // i:<int64> -> openid（反查）
	bucketMsgMeta  = []byte("msgmeta")  // <int64 message_id> -> json(MsgMeta)
	bucketC2CState = []byte("c2cstate") // openid -> json(C2CMsgState)
	bucketMediaImg = []byte("mediaimg") // image_id -> json(MediaImageRecord)
	bucketMsgTime  = []byte("msgtime")  // <8 字节 unix 秒><message_id 十进制> -> 空：按时间清理 msgmeta 用的索引
	bucketInfo     = []byte("info")     // 内部标记，例如 msgtime 索引是否已为旧数据补建
)

// MediaImageRecord 框架上传过的素材图片记录（官方接口不返回 md5/sha1，需本地记账才能按内容查重）。
// 以 image_id 为键：force 重传等情况下同一 sha1 可能对应多条记录。
type MediaImageRecord struct {
	ImageID    string   `json:"image_id"`    // 官方素材 ID
	SHA1       string   `json:"sha1"`        // 图片内容 sha1（hex）
	URL        string   `json:"url"`         // qqbot-media:// 引用链接
	FileNames  []string `json:"file_names"`  // 上传 / 复用时传入过的原始文件名（本地文件名 / QQ 下发 file_name）
	UploadedAt int64    `json:"uploaded_at"` // 上传时间(秒)
}

// JoinRequestMeta 入群申请上下文，供 set_group_add_request 按 flag 反查审批参数
type JoinRequestMeta struct {
	GroupOpenID   string    // 群 openid
	MemberOpenID  string    // 申请人 openid
	JoinRequestID string    // 官方申请 ID（通常与 flag 相同）
	at            time.Time // 登记时间，供 janitor 兜底回收
}

// C2CMsgState 用户 C2C 主动消息推送开关状态（由 C2C_MSG_REJECT/RECEIVE 事件落库）
type C2CMsgState struct {
	OpenID    string `json:"openid"`
	Allow     bool   `json:"allow"`      // true=开启主动消息
	UpdatedAt int64  `json:"updated_at"` // 事件时间戳(秒)
}

// Store openid<->int64 映射、message_id 映射与 lazy 池。
// 内存 sync.Map 作缓存，db 非空时写穿/读穿持久化。
type Store struct {
	id2open sync.Map // int64  -> string(openid)
	open2id sync.Map // string -> int64

	msg2meta sync.Map // int64 -> MsgMeta

	joinReq sync.Map // string(flag=join_request_id) -> JoinRequestMeta

	c2cState sync.Map // string(openid) -> C2CMsgState

	mediaImg sync.Map // string(image_id) -> MediaImageRecord（纯内存模式的唯一存储；持久化模式下 db 为准）

	lazy       sync.Map // string(key) -> *lazyEntry
	seqCounter sync.Map // string(msgID) -> *seqEntry

	groupBoost  sync.Map // string(group_openid) -> bool：机器人开发者在这个群是群主或管理员
	activeCount sync.Map // string("g:"/"c:" + openid) -> *activeEntry：今天已发的主动消息条数（仅内存）

	db *bolt.DB // nil = 纯内存模式
	opt Options // 持久化模式的清理设置

	statsMu    sync.Mutex     // 保护 statsCache
	statsAt    time.Time      // statsCache 的计算时间
	statsCache map[string]any // Stats 的结果缓存 30 秒（数条数要遍历整个 bucket）

	stopOnce sync.Once     // 保证 janitor 仅停止一次
	stopCh   chan struct{} // 关闭时通知 janitor 退出
	kick     chan struct{} // 新增条目达到 sweepEvery 时通知 janitor 提前清理
	inserts  atomic.Int64  // 上次清理以来新增的条目数（近似值）
}

// noteInsert 记一条新增条目，攒够 sweepEvery 条时让 janitor 提前清理
func (s *Store) noteInsert() {
	if s.inserts.Add(1) < sweepEvery {
		return
	}
	s.inserts.Store(0)
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// New 创建纯内存映射存储（不持久化）
func New() *Store {
	s := &Store{stopCh: make(chan struct{}), kick: make(chan struct{}, 1)}
	s.startJanitor()
	return s
}

// Options 持久化模式的清理设置
type Options struct {
	// KeepDays 消息记录保留天数，0=不按天数删除
	KeepDays int
	// MaxBytes idmap.db 文件大小上限（字节），0=不限制。超出时从最早的消息记录删起；
	// 删掉后空出来的空间会被新数据复用，启动时空闲空间多的话会整理一次让文件变小
	MaxBytes int64
}

// Open 创建带 bbolt 持久化的映射存储；文件不存在则创建，存在则直接加载。
func Open(path string, opts ...Options) (*Store, error) {
	var opt Options
	if len(opts) > 0 {
		opt = opts[0]
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	db, err := openDB(path)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db, opt: opt, stopCh: make(chan struct{}), kick: make(chan struct{}, 1)}
	// 启动时先按设置清理一遍；空出来的空间很多时整理文件，让它真正变小
	s.indexOldMsgMeta()
	s.pruneDB(time.Now())
	if s.shouldCompact() {
		before, _ := fileSize(path)
		if err := s.compact(path); err != nil {
			log.Printf("idmap: 整理 %s 失败（不影响使用）：%v", path, err)
		} else if after, ok := fileSize(path); ok {
			log.Printf("idmap: 已整理 %s：%s → %s", path, sizeText(before), sizeText(after))
		}
		if s.db == nil {
			// 整理失败且原文件也打不开
			return nil, fmt.Errorf("整理后重新打开 %s 失败", path)
		}
	}
	s.startJanitor()
	return s, nil
}

// openDB 打开 bbolt 文件并建好需要的 bucket
func openDB(path string) (*bolt.DB, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 3 * time.Second})
	if err != nil {
		return nil, err
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{bucketIDs, bucketMsgMeta, bucketC2CState, bucketMediaImg, bucketMsgTime, bucketInfo} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func fileSize(path string) (int64, bool) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	return st.Size(), true
}

func sizeText(n int64) string {
	return fmt.Sprintf("%.1f MB", float64(n)/1024/1024)
}

// dbSizes 返回 idmap.db 文件大小和其中实际存着数据的大小（文件大小减去空闲页）
func (s *Store) dbSizes() (file, used int64) {
	_ = s.db.View(func(tx *bolt.Tx) error {
		file = tx.Size()
		return nil
	})
	st := s.db.Stats()
	free := int64(st.FreePageN+st.PendingPageN) * int64(s.db.Info().PageSize)
	used = file - free
	if used < 0 {
		used = 0
	}
	return file, used
}

// shouldCompact 空闲空间超过一半（且文件不算太小），或者文件超过了大小上限、又有不少空闲空间时，值得整理一次
func (s *Store) shouldCompact() bool {
	file, used := s.dbSizes()
	free := file - used
	if file >= 32<<20 && free*2 > file {
		return true
	}
	return s.opt.MaxBytes > 0 && file > s.opt.MaxBytes && free*4 > file
}

// compact 把 db 整理成一个新文件（只拷贝有数据的部分）再换掉原文件。只在启动时、janitor 还没运行时调用
func (s *Store) compact(path string) error {
	if err := s.db.Close(); err != nil {
		return err
	}
	s.db = nil
	tmp := path + ".compact"
	_ = os.Remove(tmp)
	err := func() error {
		src, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 3 * time.Second, ReadOnly: true})
		if err != nil {
			return err
		}
		defer src.Close()
		dst, err := bolt.Open(tmp, 0o600, &bolt.Options{Timeout: 3 * time.Second})
		if err != nil {
			return err
		}
		if err := bolt.Compact(dst, src, 64<<20); err != nil {
			_ = dst.Close()
			return err
		}
		return dst.Close()
	}()
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	// 不论整理成没成功，都重新打开（失败时打开的是原文件）
	db, oerr := openDB(path)
	if oerr != nil {
		return fmt.Errorf("%v；重新打开也失败：%v", err, oerr)
	}
	s.db = db
	return err
}

// Stats 给管理页面看的 idmap 情况。数条数要遍历整个 bucket，结果缓存 30 秒
func (s *Store) Stats() map[string]any {
	out := map[string]any{"persistent": s.db != nil}
	if s.db == nil {
		return out
	}
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	if s.statsCache != nil && time.Since(s.statsAt) < 30*time.Second {
		return s.statsCache
	}
	defer func() {
		s.statsCache, s.statsAt = out, time.Now()
	}()
	file, used := s.dbSizes()
	out["file_bytes"] = file
	out["used_bytes"] = used
	out["keep_days"] = s.opt.KeepDays
	out["max_bytes"] = s.opt.MaxBytes
	_ = s.db.View(func(tx *bolt.Tx) error {
		out["messages"] = tx.Bucket(bucketMsgMeta).Stats().KeyN
		out["ids"] = tx.Bucket(bucketIDs).Stats().KeyN
		return nil
	})
	return out
}

// Close 停止后台清理并关闭底层 db（纯内存模式仅停止 janitor）
func (s *Store) Close() error {
	s.stopOnce.Do(func() {
		if s.stopCh != nil {
			close(s.stopCh)
		}
	})
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}

// hashID 对字符串做 FNV64a 正整数哈希（确定性，用于由 openid/msg_id 生成稳定数字 id）
func hashID(openid string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(openid))
	return int64(h.Sum64() & 0x7FFFFFFFFFFFFFFF)
}

// ID 把 openid 稳定映射为正整数（确定性哈希），并登记反查
func (s *Store) ID(openid string) int64 {
	if openid == "" {
		return 0
	}
	if v, ok := s.open2id.Load(openid); ok {
		return v.(int64)
	}
	id := hashID(openid)
	// 极小概率哈希落到 0，避让
	if id == 0 {
		id = 1
	}
	if _, loaded := s.open2id.LoadOrStore(openid, id); !loaded {
		s.noteInsert()
	}
	s.id2open.Store(id, openid)
	if s.db != nil {
		// 内存缓存被淘汰后再次用到：db 里已经有了就不用再写
		var saved bool
		_ = s.db.View(func(tx *bolt.Tx) error {
			saved = string(tx.Bucket(bucketIDs).Get(idKey(id))) == openid
			return nil
		})
		if !saved {
			_ = s.db.Update(func(tx *bolt.Tx) error {
				return tx.Bucket(bucketIDs).Put(idKey(id), []byte(openid))
			})
		}
	}
	return id
}

// OpenID 反查 int64 对应的 openid（内存未命中时回落 db）
func (s *Store) OpenID(id int64) (string, bool) {
	if v, ok := s.id2open.Load(id); ok {
		return v.(string), true
	}
	if s.db == nil {
		return "", false
	}
	var openid string
	_ = s.db.View(func(tx *bolt.Tx) error {
		if v := tx.Bucket(bucketIDs).Get(idKey(id)); v != nil {
			openid = string(v)
		}
		return nil
	})
	if openid == "" {
		return "", false
	}
	s.id2open.Store(id, openid)
	s.open2id.Store(openid, id)
	return openid, true
}

// AllocMsgID 由 QQ msg_id 哈希出 message_id（确定性），并保存元信息
func (s *Store) AllocMsgID(meta MsgMeta) int64 {
	if meta.MsgID == "" {
		return 0
	}
	id := hashID(meta.MsgID)
	if id == 0 {
		id = 1
	}
	if meta.Time.IsZero() {
		meta.Time = time.Now()
	}
	if _, loaded := s.msg2meta.Swap(id, meta); !loaded {
		s.noteInsert()
	}
	if s.db != nil {
		if b, err := json.Marshal(meta); err == nil {
			_ = s.db.Update(func(tx *bolt.Tx) error {
				if err := tx.Bucket(bucketMsgMeta).Put(idKey(id), b); err != nil {
					return err
				}
				return tx.Bucket(bucketMsgTime).Put(msgTimeKey(meta.Time, idKey(id)), nil)
			})
		}
	}
	return id
}

// msgTimeKey msgtime 索引的 key：8 字节时间在前，按时间顺序排列，清理时从头删到过期时间即可；后面是 msgmeta 的 key
func msgTimeKey(t time.Time, metaKey []byte) []byte {
	k := make([]byte, 8, 8+len(metaKey))
	binary.BigEndian.PutUint64(k, uint64(t.Unix()))
	return append(k, metaKey...)
}

// Msg 反查 message_id 的元信息（内存未命中时回落 db）
func (s *Store) Msg(id int64) (MsgMeta, bool) {
	if v, ok := s.msg2meta.Load(id); ok {
		return v.(MsgMeta), true
	}
	if s.db == nil {
		return MsgMeta{}, false
	}
	var meta MsgMeta
	found := false
	_ = s.db.View(func(tx *bolt.Tx) error {
		if v := tx.Bucket(bucketMsgMeta).Get(idKey(id)); v != nil {
			if err := json.Unmarshal(v, &meta); err == nil {
				found = true
			}
		}
		return nil
	})
	if !found {
		return MsgMeta{}, false
	}
	s.msg2meta.Store(id, meta)
	return meta, true
}

// idKey 把 int64 id 编码为 bbolt key
func idKey(id int64) []byte {
	return []byte(strconv.FormatInt(id, 10))
}

// StoreLazyGroup 记录群最近一次收到的 msg_id（同时存 group 与 group+user 维度）
func (s *Store) StoreLazyGroup(groupOpenID, userOpenID, msgID string) {
	s.storeLazyGroup(groupOpenID, userOpenID, msgID, "msg")
}

// StoreLazyGroupEvent 记录群最近一次互动事件 event_id（被动回复凭证）
func (s *Store) StoreLazyGroupEvent(groupOpenID, userOpenID, eventID string) {
	s.storeLazyGroup(groupOpenID, userOpenID, eventID, "event")
}

// storeLazyGroup 按 group 与 group+user 两个维度记录被动回复凭证（kind=msg/event）
func (s *Store) storeLazyGroup(groupOpenID, userOpenID, id, kind string) {
	if groupOpenID == "" || id == "" {
		return
	}
	now := time.Now()
	if _, loaded := s.lazy.Swap("g:"+groupOpenID, &lazyEntry{id, kind, now}); !loaded {
		s.noteInsert()
	}
	if userOpenID != "" {
		if _, loaded := s.lazy.Swap("gu:"+groupOpenID+":"+userOpenID, &lazyEntry{id, kind, now}); !loaded {
			s.noteInsert()
		}
	}
}

// StoreLazyC2C 记录单聊最近一次收到的 msg_id
func (s *Store) StoreLazyC2C(userOpenID, msgID string) {
	s.storeLazyC2C(userOpenID, msgID, "msg")
}

// StoreLazyC2CEvent 记录单聊最近一次互动事件 event_id（被动回复凭证）
func (s *Store) StoreLazyC2CEvent(userOpenID, eventID string) {
	s.storeLazyC2C(userOpenID, eventID, "event")
}

// storeLazyC2C 记录单聊被动回复凭证（kind=msg/event）
func (s *Store) storeLazyC2C(userOpenID, id, kind string) {
	if userOpenID == "" || id == "" {
		return
	}
	if _, loaded := s.lazy.Swap("c:"+userOpenID, &lazyEntry{id, kind, time.Now()}); !loaded {
		s.noteInsert()
	}
}

// loadLazy 读取并校验某 key 的被动凭证，超过 ttl 视为过期失效
func (s *Store) loadLazy(key string, ttl time.Duration) (string, string, bool) {
	v, ok := s.lazy.Load(key)
	if !ok {
		return "", "", false
	}
	e := v.(*lazyEntry)
	if ttl > 0 && time.Since(e.at) > ttl {
		return "", "", false
	}
	return e.msgID, e.kind, true
}

// LazyGroup 取群被动回复目标凭证，优先 group+user 维度；返回 (id, kind)
func (s *Store) LazyGroup(groupOpenID, userOpenID string) (string, string, bool) {
	if userOpenID != "" {
		if id, kind, ok := s.loadLazy("gu:"+groupOpenID+":"+userOpenID, GroupPassiveTTL); ok {
			return id, kind, true
		}
	}
	return s.loadLazy("g:"+groupOpenID, GroupPassiveTTL)
}

// SetGroupBoost 记下机器人开发者在这个群是不是群主或管理员（影响每条群消息可被动回复的次数）
func (s *Store) SetGroupBoost(groupOpenID string, boosted bool) {
	if groupOpenID == "" {
		return
	}
	if boosted {
		s.groupBoost.Store(groupOpenID, true)
	} else {
		s.groupBoost.Delete(groupOpenID)
	}
}

// GroupBoosted 机器人开发者在这个群是不是群主或管理员
func (s *Store) GroupBoosted(groupOpenID string) bool {
	_, ok := s.groupBoost.Load(groupOpenID)
	return ok
}

// PassiveState 被动回复凭证的状态
type PassiveState struct {
	Available   bool      // 凭证还在有效期内
	Kind        string    // msg / event
	ReceivedAt  time.Time // 凭证对应的消息（事件）收到的时间
	ExpiresAt   time.Time // 过期时间
	TTL         time.Duration
	RepliesUsed int // 这条凭证已经被动回复的次数
	RepliesMax  int // 每条凭证最多可回复次数
	Boosted     bool
}

// GroupPassiveState 群最近一条被动凭证（群维度）的状态；没有收到过消息时 ReceivedAt 为零值
func (s *Store) GroupPassiveState(groupOpenID string) PassiveState {
	st := PassiveState{TTL: GroupPassiveTTL, RepliesMax: GroupPassiveReplies, Boosted: s.GroupBoosted(groupOpenID)}
	if st.Boosted {
		st.RepliesMax = GroupPassiveRepliesBoost
	}
	s.fillPassive(&st, "g:"+groupOpenID)
	return st
}

// C2CPassiveState 单聊最近一条被动凭证的状态
func (s *Store) C2CPassiveState(userOpenID string) PassiveState {
	st := PassiveState{TTL: C2CPassiveTTL, RepliesMax: C2CPassiveReplies}
	s.fillPassive(&st, "c:"+userOpenID)
	return st
}

func (s *Store) fillPassive(st *PassiveState, key string) {
	v, ok := s.lazy.Load(key)
	if !ok {
		return
	}
	e := v.(*lazyEntry)
	st.Kind = e.kind
	st.ReceivedAt = e.at
	st.ExpiresAt = e.at.Add(st.TTL)
	st.RepliesUsed = s.SeqCount(e.msgID)
	st.Available = time.Now().Before(st.ExpiresAt) && st.RepliesUsed < st.RepliesMax
}

// SeqCount 某个 msg_id / event_id 已经分配过的 msg_seq 个数（即已被动回复的次数）
func (s *Store) SeqCount(msgID string) int {
	v, ok := s.seqCounter.Load(msgID)
	if !ok {
		return 0
	}
	return int(atomic.LoadInt32(&v.(*seqEntry).n))
}

// activeEntry 某个会话今天的主动消息计数
type activeEntry struct {
	mu  sync.Mutex
	day string
	n   int
}

// AddActive 记一条成功发出的主动消息；scene 为 group / c2c
func (s *Store) AddActive(scene Scene, openid string) {
	e := s.activeEntry(scene, openid)
	e.mu.Lock()
	defer e.mu.Unlock()
	today := time.Now().Format("2006-01-02")
	if e.day != today {
		e.day, e.n = today, 0
	}
	e.n++
}

// ActiveToday 今天已经发出的主动消息条数（goqbot 重启后从 0 重新计数）
func (s *Store) ActiveToday(scene Scene, openid string) int {
	e := s.activeEntry(scene, openid)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.day != time.Now().Format("2006-01-02") {
		return 0
	}
	return e.n
}

func (s *Store) activeEntry(scene Scene, openid string) *activeEntry {
	v, _ := s.activeCount.LoadOrStore(string(scene)+":"+openid, &activeEntry{})
	return v.(*activeEntry)
}

// LazyC2C 取单聊被动回复目标凭证；返回 (id, kind)
func (s *Store) LazyC2C(userOpenID string) (string, string, bool) {
	return s.loadLazy("c:"+userOpenID, C2CPassiveTTL)
}

// NextSeq 针对某个 msg_id 取下一个序号（从 1 开始），用于 msg_seq 去重
func (s *Store) NextSeq(msgID string) uint32 {
	v, loaded := s.seqCounter.LoadOrStore(msgID, &seqEntry{})
	if !loaded {
		s.noteInsert()
	}
	e := v.(*seqEntry)
	atomic.StoreInt64(&e.last, time.Now().UnixNano())
	return uint32(atomic.AddInt32(&e.n, 1))
}

// PutJoinRequest 登记入群申请 flag→上下文（仅内存，审批后可 DeleteJoinRequest）
func (s *Store) PutJoinRequest(flag string, meta JoinRequestMeta) {
	if flag == "" || meta.GroupOpenID == "" || meta.MemberOpenID == "" {
		return
	}
	if meta.JoinRequestID == "" {
		meta.JoinRequestID = flag
	}
	meta.at = time.Now()
	s.joinReq.Store(flag, meta)
}

// GetJoinRequest 按 flag 取回入群申请上下文
func (s *Store) GetJoinRequest(flag string) (JoinRequestMeta, bool) {
	if flag == "" {
		return JoinRequestMeta{}, false
	}
	v, ok := s.joinReq.Load(flag)
	if !ok {
		return JoinRequestMeta{}, false
	}
	return v.(JoinRequestMeta), true
}

// DeleteJoinRequest 清除已处理的入群申请 flag
func (s *Store) DeleteJoinRequest(flag string) {
	if flag == "" {
		return
	}
	s.joinReq.Delete(flag)
}

// PutC2CMsgState 登记用户 C2C 主动消息推送开关状态（写内存，db 非空时持久化）
func (s *Store) PutC2CMsgState(openid string, allow bool, ts int64) {
	if openid == "" {
		return
	}
	st := C2CMsgState{OpenID: openid, Allow: allow, UpdatedAt: ts}
	s.c2cState.Store(openid, st)
	if s.db != nil {
		if b, err := json.Marshal(st); err == nil {
			_ = s.db.Update(func(tx *bolt.Tx) error {
				return tx.Bucket(bucketC2CState).Put([]byte(openid), b)
			})
		}
	}
}

// GetC2CMsgState 按 openid 取回 C2C 推送开关状态（内存未命中时回落 db）
func (s *Store) GetC2CMsgState(openid string) (C2CMsgState, bool) {
	if openid == "" {
		return C2CMsgState{}, false
	}
	if v, ok := s.c2cState.Load(openid); ok {
		return v.(C2CMsgState), true
	}
	if s.db == nil {
		return C2CMsgState{}, false
	}
	var st C2CMsgState
	found := false
	_ = s.db.View(func(tx *bolt.Tx) error {
		if v := tx.Bucket(bucketC2CState).Get([]byte(openid)); v != nil {
			if err := json.Unmarshal(v, &st); err == nil {
				found = true
			}
		}
		return nil
	})
	if !found {
		return C2CMsgState{}, false
	}
	s.c2cState.Store(openid, st)
	return st, true
}

// PutMediaImage 登记素材图片上传记录（同一 image_id 覆盖旧记录）
func (s *Store) PutMediaImage(rec MediaImageRecord) {
	if rec.ImageID == "" {
		return
	}
	s.mediaImg.Store(rec.ImageID, rec)
	if s.db != nil {
		if b, err := json.Marshal(rec); err == nil {
			_ = s.db.Update(func(tx *bolt.Tx) error {
				return tx.Bucket(bucketMediaImg).Put([]byte(rec.ImageID), b)
			})
		}
	}
}

// MediaImagesBySHA1 按内容 sha1 取回素材记录，最近上传的在前
func (s *Store) MediaImagesBySHA1(sha1 string) []MediaImageRecord {
	var out []MediaImageRecord
	for _, rec := range s.MediaImages() {
		if strings.EqualFold(rec.SHA1, sha1) {
			out = append(out, rec)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UploadedAt > out[j].UploadedAt })
	return out
}

// AddMediaImageName 给已有素材记录追加一个文件名（同内容以新文件名复用时调用，便于后续按名查询）
func (s *Store) AddMediaImageName(imageID, name string) {
	if name == "" {
		return
	}
	for _, rec := range s.MediaImages() {
		if rec.ImageID != imageID {
			continue
		}
		for _, n := range rec.FileNames {
			if n == name {
				return
			}
		}
		rec.FileNames = append(rec.FileNames, name)
		s.PutMediaImage(rec)
		return
	}
}

// MediaImages 返回全部素材记录（持久化模式读 db，否则读内存）。单机器人上限 100 张，全量扫描即可。
func (s *Store) MediaImages() []MediaImageRecord {
	var out []MediaImageRecord
	if s.db == nil {
		s.mediaImg.Range(func(_, v any) bool {
			out = append(out, v.(MediaImageRecord))
			return true
		})
		return out
	}
	_ = s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketMediaImg).ForEach(func(_, v []byte) error {
			var rec MediaImageRecord
			if err := json.Unmarshal(v, &rec); err == nil {
				out = append(out, rec)
			}
			return nil
		})
	})
	return out
}

// DeleteMediaImage 按官方 image_id 删除素材记录
func (s *Store) DeleteMediaImage(imageID string) {
	if imageID == "" {
		return
	}
	s.mediaImg.Delete(imageID)
	if s.db != nil {
		_ = s.db.Update(func(tx *bolt.Tx) error {
			return tx.Bucket(bucketMediaImg).Delete([]byte(imageID))
		})
	}
}

// startJanitor 启动后台清理协程，周期性回收具有时效性的内存条目。
func (s *Store) startJanitor() {
	go func() {
		t := time.NewTicker(janitorInterval)
		defer t.Stop()
		lastPrune := time.Now()
		var lastFree time.Time
		for {
			var now time.Time
			select {
			case <-s.stopCh:
				return
			case now = <-t.C:
			case <-s.kick:
				now = time.Now()
			}
			s.inserts.Store(0)
			removed := s.sweepMsgMeta(now) + s.sweepLazy(now) + s.sweepSeq(now) + s.sweepJoinReq(now) + s.sweepIDs() + s.sweepActive(now)
			if s.db != nil && now.Sub(lastPrune) >= dbPruneInterval {
				lastPrune = now
				s.pruneDB(now)
			}
			// 一次回收了很多条目时，把空出来的内存还给系统（最多 5 分钟一次）
			if removed >= freeOSAfter && now.Sub(lastFree) >= janitorInterval {
				lastFree = now
				debug.FreeOSMemory()
			}
		}
	}()
}

// sweepIDs 持久化模式下，内存里缓存的 openid 超过上限时淘汰一部分（用到时 ID / OpenID 会从 db 读回）。
// 纯内存模式下这是唯一的记录，不清理。返回淘汰的条数
func (s *Store) sweepIDs() int {
	if s.db == nil {
		return 0
	}
	count := func(m *sync.Map) int {
		n := 0
		m.Range(func(_, _ any) bool {
			n++
			return true
		})
		return n
	}
	removed := 0
	if n := count(&s.open2id); n > idCacheCap {
		s.open2id.Range(func(k, v any) bool {
			if n <= idCacheKeep {
				return false
			}
			s.open2id.Delete(k)
			s.id2open.Delete(v)
			n--
			removed++
			return true
		})
	}
	// id2open 里还可能有只被 OpenID 读回的条目，也按上限收一下
	if n := count(&s.id2open); n > idCacheCap {
		s.id2open.Range(func(k, v any) bool {
			if n <= idCacheKeep {
				return false
			}
			s.id2open.Delete(k)
			s.open2id.Delete(v)
			n--
			removed++
			return true
		})
	}
	return removed
}

// sweepActive 回收不是今天的主动消息计数。返回回收的条数
func (s *Store) sweepActive(now time.Time) int {
	today := now.Format("2006-01-02")
	removed := 0
	s.activeCount.Range(func(k, v any) bool {
		e := v.(*activeEntry)
		e.mu.Lock()
		stale := e.day != today
		e.mu.Unlock()
		if stale {
			s.activeCount.Delete(k)
			removed++
		}
		return true
	})
	return removed
}

// pruneDB 按设置清理 db 里的消息元信息：先删超过保留天数的，再看文件大小上限，超出时从最早的删起
func (s *Store) pruneDB(now time.Time) {
	if s.opt.KeepDays > 0 {
		s.pruneBefore(uint64(now.Add(-time.Duration(s.opt.KeepDays) * 24 * time.Hour).Unix()))
	}
	if s.opt.MaxBytes > 0 {
		s.pruneToSize()
	}
}

// pruneToSize 存着的数据超过大小上限时，从最早的消息记录删起，删到上限的 90% 左右。
// 删除后数据页不会马上合并，不能边删边看文件里的数据量，所以先按每条消息记录平均占多少空间算出要删几条
func (s *Store) pruneToSize() {
	_, used := s.dbSizes()
	if used <= s.opt.MaxBytes {
		return
	}
	// 按两个 bucket 实际占的页数算（页往往只填了一半左右，按页里的数据量算会删多）
	var keys int
	var bytes int64
	page := int64(s.db.Info().PageSize)
	_ = s.db.View(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{bucketMsgMeta, bucketMsgTime} {
			st := tx.Bucket(name).Stats()
			bytes += int64(st.LeafPageN+st.LeafOverflowN+st.BranchPageN+st.BranchOverflowN) * page
			if string(name) == string(bucketMsgMeta) {
				keys = st.KeyN
			}
		}
		return nil
	})
	if keys == 0 {
		log.Printf("idmap: 没有可删的消息记录，但 ID 对照本身已超过大小上限 %s", sizeText(s.opt.MaxBytes))
		return
	}
	per := bytes / int64(keys)
	if per <= 0 {
		per = 1
	}
	need := int((used - s.opt.MaxBytes/10*9 + per - 1) / per)
	if need > keys {
		need = keys
	}
	deleted := 0
	for deleted < need {
		n := s.deleteOldest(^uint64(0), min(5000, need-deleted))
		if n == 0 {
			break
		}
		deleted += n
	}
	log.Printf("idmap: 数据超过大小上限 %s，已删除最早的 %d 条消息记录", sizeText(s.opt.MaxBytes), deleted)
}

// pruneBefore 删掉时间早于 cutoff（unix 秒）的消息元信息，每批 5000 条
func (s *Store) pruneBefore(cutoff uint64) {
	for s.deleteOldest(cutoff, 5000) == 5000 {
	}
}

// deleteOldest 按 msgtime 索引从最早的删起，最多删 limit 条时间早于 cutoff 的，返回删掉的条数
func (s *Store) deleteOldest(cutoff uint64, limit int) int {
	var keys [][]byte
	_ = s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(bucketMsgTime).Cursor()
		for k, _ := c.First(); k != nil && len(keys) < limit; k, _ = c.Next() {
			if len(k) <= 8 || binary.BigEndian.Uint64(k[:8]) >= cutoff {
				break
			}
			keys = append(keys, append([]byte(nil), k...))
		}
		return nil
	})
	if len(keys) == 0 {
		return 0
	}
	err := s.db.Update(func(tx *bolt.Tx) error {
		idx, meta := tx.Bucket(bucketMsgTime), tx.Bucket(bucketMsgMeta)
		for _, k := range keys {
			if err := idx.Delete(k); err != nil {
				return err
			}
			if err := meta.Delete(k[8:]); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0
	}
	return len(keys)
}

// indexOldMsgMeta 给旧版本写进 db、还没有 msgtime 索引的消息元信息补建索引（只做一次）。
// 已经超过保留时长的直接删掉
func (s *Store) indexOldMsgMeta() {
	done := false
	_ = s.db.View(func(tx *bolt.Tx) error {
		done = tx.Bucket(bucketInfo).Get([]byte("msgtime_indexed")) != nil
		return nil
	})
	if done {
		return
	}
	var cutoff time.Time // 零值=不按天数删除
	if s.opt.KeepDays > 0 {
		cutoff = time.Now().Add(-time.Duration(s.opt.KeepDays) * 24 * time.Hour)
	}
	type item struct {
		key  []byte
		t    time.Time
		drop bool
	}
	var items []item
	_ = s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketMsgMeta).ForEach(func(k, v []byte) error {
			var meta struct{ Time time.Time }
			if json.Unmarshal(v, &meta) != nil || meta.Time.IsZero() {
				items = append(items, item{key: append([]byte(nil), k...), drop: true})
				return nil
			}
			items = append(items, item{key: append([]byte(nil), k...), t: meta.Time, drop: !cutoff.IsZero() && meta.Time.Before(cutoff)})
			return nil
		})
	})
	for start := 0; start < len(items); start += 5000 {
		batch := items[start:min(start+5000, len(items))]
		_ = s.db.Update(func(tx *bolt.Tx) error {
			idx, meta := tx.Bucket(bucketMsgTime), tx.Bucket(bucketMsgMeta)
			for _, it := range batch {
				if it.drop {
					_ = meta.Delete(it.key)
					continue
				}
				_ = idx.Put(msgTimeKey(it.t, it.key), nil)
			}
			return nil
		})
	}
	_ = s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketInfo).Put([]byte("msgtime_indexed"), []byte("1"))
	})
}

// sweepMsgMeta 回收内存中过期的消息元信息，并在超出硬上限时淘汰最旧。
// 注意：仅清理内存副本；持久化模式下 db 仍保留，后续 Msg() 可按需回落重载。
func (s *Store) sweepMsgMeta(now time.Time) int {
	removed := 0
	type kt struct {
		k int64
		t time.Time
	}
	kept := make([]kt, 0)
	s.msg2meta.Range(func(k, v any) bool {
		meta := v.(MsgMeta)
		if msgMetaTTL > 0 && now.Sub(meta.Time) > msgMetaTTL {
			s.msg2meta.Delete(k)
			removed++
			return true
		}
		kept = append(kept, kt{k.(int64), meta.Time})
		return true
	})
	if msgMetaCap > 0 && len(kept) > msgMetaCap {
		sort.Slice(kept, func(i, j int) bool { return kept[i].t.Before(kept[j].t) })
		for _, e := range kept[:len(kept)-msgMetaCap] {
			s.msg2meta.Delete(e.k)
			removed++
		}
	}
	return removed
}

// sweepLazy 回收过期的被动回复凭证（按 key 前缀区分群/单聊 TTL），并在超限时淘汰最旧。
func (s *Store) sweepLazy(now time.Time) int {
	removed := 0
	type kt struct {
		k string
		t time.Time
	}
	kept := make([]kt, 0)
	s.lazy.Range(func(k, v any) bool {
		key := k.(string)
		e := v.(*lazyEntry)
		ttl := GroupPassiveTTL
		if strings.HasPrefix(key, "c:") {
			ttl = C2CPassiveTTL
		}
		if ttl > 0 && now.Sub(e.at) > ttl {
			s.lazy.Delete(k)
			removed++
			return true
		}
		kept = append(kept, kt{key, e.at})
		return true
	})
	if lazyCap > 0 && len(kept) > lazyCap {
		sort.Slice(kept, func(i, j int) bool { return kept[i].t.Before(kept[j].t) })
		for _, e := range kept[:len(kept)-lazyCap] {
			s.lazy.Delete(e.k)
			removed++
		}
	}
	return removed
}

// sweepSeq 回收空闲的 msg_seq 计数器，并在超限时淘汰最旧。
func (s *Store) sweepSeq(now time.Time) int {
	removed := 0
	type kt struct {
		k string
		t int64
	}
	kept := make([]kt, 0)
	nowNano := now.UnixNano()
	s.seqCounter.Range(func(k, v any) bool {
		e := v.(*seqEntry)
		last := atomic.LoadInt64(&e.last)
		if seqTTL > 0 && nowNano-last > int64(seqTTL) {
			s.seqCounter.Delete(k)
			removed++
			return true
		}
		kept = append(kept, kt{k.(string), last})
		return true
	})
	if seqCap > 0 && len(kept) > seqCap {
		sort.Slice(kept, func(i, j int) bool { return kept[i].t < kept[j].t })
		for _, e := range kept[:len(kept)-seqCap] {
			s.seqCounter.Delete(e.k)
			removed++
		}
	}
	return removed
}

// sweepJoinReq 兜底回收长期未处理的入群申请上下文（正常审批后已由 DeleteJoinRequest 清除）。
func (s *Store) sweepJoinReq(now time.Time) int {
	removed := 0
	s.joinReq.Range(func(k, v any) bool {
		m := v.(JoinRequestMeta)
		if joinReqTTL > 0 && !m.at.IsZero() && now.Sub(m.at) > joinReqTTL {
			s.joinReq.Delete(k)
			removed++
		}
		return true
	})
	return removed
}
