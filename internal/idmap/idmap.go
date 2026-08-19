// Package idmap 负责 QQ string openid 与 OneBot int64 id 之间的映射，
// 以及 message_id 的确定性哈希分配、被动回复所需的原始 msg_id 池。
// 可选用 bbolt 持久化（反向 openid 与 message_id 元信息），使重启后仍可解析。
package idmap

import (
	"encoding/json"
	"hash/fnv"
	"os"
	"path/filepath"
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

// 被动回复有效期（保守取值，留出网络余量）
const (
	// GroupPassiveTTL 群被动回复有效期
	GroupPassiveTTL = 5 * time.Minute
	// C2CPassiveTTL 单聊被动回复有效期
	C2CPassiveTTL = 60 * time.Minute
)

// 后台清理（janitor）参数：控制长期运行时内存内几个只增不减的 sync.Map。
// 说明：id2open/open2id/c2cState 属身份/状态映射，语义上需长期驻留，不做清理；
// 这里只回收具有时效性的 msg2meta / lazy / seqCounter / joinReq。
const (
	// janitorInterval 后台清理扫描周期
	janitorInterval = 5 * time.Minute
	// msgMetaTTL 内存中消息元信息保留时长（超时后从内存淘汰；持久化模式下仍可回落 db 重载）
	msgMetaTTL = 2 * time.Hour
	// msgMetaCap 内存中消息元信息条数硬上限（超出按时间淘汰最旧）
	msgMetaCap = 50000
	// lazyCap 被动回复凭证条数硬上限
	lazyCap = 100000
	// seqTTL msg_seq 计数器空闲保留时长（被动窗口过后即可回收）
	seqTTL = C2CPassiveTTL
	// seqCap msg_seq 计数器条数硬上限
	seqCap = 100000
	// joinReqTTL 入群申请上下文兜底保留时长（正常审批后即删除，此为未处理时的回收阈值）
	joinReqTTL = 24 * time.Hour
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

// PassiveOK 是否还能当作被动回复凭证（入站且未过窗口）
func (m MsgMeta) PassiveOK() bool {
	if m.Outbound || m.MsgID == "" {
		return false
	}
	ttl := GroupPassiveTTL
	if m.Scene == SceneC2C {
		ttl = C2CPassiveTTL
	}
	return !m.Time.IsZero() && time.Since(m.Time) < ttl
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
)

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

	lazy       sync.Map // string(key) -> *lazyEntry
	seqCounter sync.Map // string(msgID) -> *seqEntry

	db *bolt.DB // nil = 纯内存模式

	stopOnce sync.Once     // 保证 janitor 仅停止一次
	stopCh   chan struct{} // 关闭时通知 janitor 退出
}

// New 创建纯内存映射存储（不持久化）
func New() *Store {
	s := &Store{stopCh: make(chan struct{})}
	s.startJanitor()
	return s
}

// Open 创建带 bbolt 持久化的映射存储；文件不存在则创建，存在则直接加载。
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 3 * time.Second})
	if err != nil {
		return nil, err
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{bucketIDs, bucketMsgMeta, bucketC2CState} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		_ = db.Close()
		return nil, err
	}
	s := &Store{db: db, stopCh: make(chan struct{})}
	s.startJanitor()
	return s, nil
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
	s.open2id.Store(openid, id)
	s.id2open.Store(id, openid)
	if s.db != nil {
		_ = s.db.Update(func(tx *bolt.Tx) error {
			return tx.Bucket(bucketIDs).Put(idKey(id), []byte(openid))
		})
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
	s.msg2meta.Store(id, meta)
	if s.db != nil {
		if b, err := json.Marshal(meta); err == nil {
			_ = s.db.Update(func(tx *bolt.Tx) error {
				return tx.Bucket(bucketMsgMeta).Put(idKey(id), b)
			})
		}
	}
	return id
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
	s.lazy.Store("g:"+groupOpenID, &lazyEntry{id, kind, now})
	if userOpenID != "" {
		s.lazy.Store("gu:"+groupOpenID+":"+userOpenID, &lazyEntry{id, kind, now})
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
	s.lazy.Store("c:"+userOpenID, &lazyEntry{id, kind, time.Now()})
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

// LazyC2C 取单聊被动回复目标凭证；返回 (id, kind)
func (s *Store) LazyC2C(userOpenID string) (string, string, bool) {
	return s.loadLazy("c:"+userOpenID, C2CPassiveTTL)
}

// NextSeq 针对某个 msg_id 取下一个序号（从 1 开始），用于 msg_seq 去重
func (s *Store) NextSeq(msgID string) uint32 {
	v, _ := s.seqCounter.LoadOrStore(msgID, &seqEntry{})
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

// startJanitor 启动后台清理协程，周期性回收具有时效性的内存条目。
func (s *Store) startJanitor() {
	go func() {
		t := time.NewTicker(janitorInterval)
		defer t.Stop()
		for {
			select {
			case <-s.stopCh:
				return
			case now := <-t.C:
				s.sweepMsgMeta(now)
				s.sweepLazy(now)
				s.sweepSeq(now)
				s.sweepJoinReq(now)
			}
		}
	}()
}

// sweepMsgMeta 回收内存中过期的消息元信息，并在超出硬上限时淘汰最旧。
// 注意：仅清理内存副本；持久化模式下 db 仍保留，后续 Msg() 可按需回落重载。
func (s *Store) sweepMsgMeta(now time.Time) {
	type kt struct {
		k int64
		t time.Time
	}
	kept := make([]kt, 0)
	s.msg2meta.Range(func(k, v any) bool {
		meta := v.(MsgMeta)
		if msgMetaTTL > 0 && now.Sub(meta.Time) > msgMetaTTL {
			s.msg2meta.Delete(k)
			return true
		}
		kept = append(kept, kt{k.(int64), meta.Time})
		return true
	})
	if msgMetaCap > 0 && len(kept) > msgMetaCap {
		sort.Slice(kept, func(i, j int) bool { return kept[i].t.Before(kept[j].t) })
		for _, e := range kept[:len(kept)-msgMetaCap] {
			s.msg2meta.Delete(e.k)
		}
	}
}

// sweepLazy 回收过期的被动回复凭证（按 key 前缀区分群/单聊 TTL），并在超限时淘汰最旧。
func (s *Store) sweepLazy(now time.Time) {
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
			return true
		}
		kept = append(kept, kt{key, e.at})
		return true
	})
	if lazyCap > 0 && len(kept) > lazyCap {
		sort.Slice(kept, func(i, j int) bool { return kept[i].t.Before(kept[j].t) })
		for _, e := range kept[:len(kept)-lazyCap] {
			s.lazy.Delete(e.k)
		}
	}
}

// sweepSeq 回收空闲的 msg_seq 计数器，并在超限时淘汰最旧。
func (s *Store) sweepSeq(now time.Time) {
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
			return true
		}
		kept = append(kept, kt{k.(string), last})
		return true
	})
	if seqCap > 0 && len(kept) > seqCap {
		sort.Slice(kept, func(i, j int) bool { return kept[i].t < kept[j].t })
		for _, e := range kept[:len(kept)-seqCap] {
			s.seqCounter.Delete(e.k)
		}
	}
}

// sweepJoinReq 兜底回收长期未处理的入群申请上下文（正常审批后已由 DeleteJoinRequest 清除）。
func (s *Store) sweepJoinReq(now time.Time) {
	s.joinReq.Range(func(k, v any) bool {
		m := v.(JoinRequestMeta)
		if joinReqTTL > 0 && !m.at.IsZero() && now.Sub(m.at) > joinReqTTL {
			s.joinReq.Delete(k)
		}
		return true
	})
}
