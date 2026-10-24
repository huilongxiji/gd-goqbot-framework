package v1

import (
	"fmt"

	"goqbot/internal/botgo/constant"
)

type uri string

// 目前提供的接口的 uri
const (
	guildURI            uri = "/guilds/{guild_id}"
	guildMembersURI     uri = "/guilds/{guild_id}/members"
	guildMemberURI      uri = "/guilds/{guild_id}/members/{user_id}"
	guildRoleMemberURI  uri = "/guilds/{guild_id}/roles/{role_id}/members"
	guildMuteURI        uri = "/guilds/{guild_id}/mute"                   // 频道禁言
	guildMembersMuteURI uri = "/guilds/{guild_id}/members/{user_id}/mute" // 频道指定成员禁言

	channelsURI uri = "/guilds/{guild_id}/channels"
	channelURI  uri = "/channels/{channel_id}"

	channelPermissionsURI      uri = "/channels/{channel_id}/members/{user_id}/permissions"
	channelRolesPermissionsURI uri = "/channels/{channel_id}/roles/{role_id}/permissions"

	messagesURI       uri = "/channels/{channel_id}/messages"
	groupMessagesURI  uri = "/v2/groups/{group_id}/messages"
	groupRichMediaURI uri = "/v2/groups/{group_id}/files"

	groupInfoURI                uri = "/v2/groups/{group_id}/info"                              // 获取群基本信息
	groupBotStateURI            uri = "/v2/groups/{group_id}/bot_state"                         // 获取机器人群内状态
	groupMembersURI             uri = "/v2/groups/{group_id}/members"                           // 获取群成员列表
	groupMemberURI              uri = "/v2/groups/{group_id}/members/{member_id}"               // 获取群成员信息
	groupJoinRequestListURI     uri = "/v2/groups/{group_id}/join_request_list"                 // 入群申请列表
	groupApprovalJoinRequestURI uri = "/v2/groups/{group_id}/approval_join_request/{member_id}" // 入群申请审批
	groupRestrictChatURI        uri = "/v2/groups/{group_id}/restrict_chat_setting"             // 查询/设置群禁言

	groupUploadPrepareURI    uri = "/v2/groups/{group_id}/upload_prepare"     // 分片上传-准备
	groupUploadPartFinishURI uri = "/v2/groups/{group_id}/upload_part_finish" // 分片上传-单片完成

	c2cMessagesURI  uri = "/v2/users/{user_id}/messages"
	c2cStreamURI    uri = "/v2/users/{user_id}/stream_messages" // 流式发送单聊消息（独立接口）
	c2cRichMediaURI uri = "/v2/users/{user_id}/files"

	c2cUploadPrepareURI    uri = "/v2/users/{user_id}/upload_prepare"     // 分片上传-准备
	c2cUploadPartFinishURI uri = "/v2/users/{user_id}/upload_part_finish" // 分片上传-单片完成

	retractC2cMessageURI   uri = "/v2/users/{user_id}/messages/{message_id}"
	retractGroupMessageURI uri = "/v2/groups/{group_id}/messages/{message_id}"

	messageURI uri = "/channels/{channel_id}/messages/{message_id}"

	userMeURI       uri = "/users/@me"
	userMeGuildsURI uri = "/users/@me/guilds"
	userMeDMURI     uri = "/users/@me/dms"

	gatewayURI    uri = "/gateway" // nolint
	gatewayBotURI uri = "/gateway/bot"

	audioControlURI uri = "/channels/{channel_id}/audio"
	micURI          uri = "/channels/{channel_id}/mic"

	rolesURI uri = "/guilds/{guild_id}/roles"
	roleURI  uri = "/guilds/{guild_id}/roles/{role_id}"

	memberRoleURI uri = "/guilds/{guild_id}/members/{user_id}/roles/{role_id}"

	dmsURI        uri = "/dms/{guild_id}/messages"
	dmsMessageURI uri = "/dms/{guild_id}/messages/{message_id}"

	channelAnnouncesURI = "/channels/{channel_id}/announces"
	channelAnnounceURI  = "/channels/{channel_id}/announces/{message_id}"

	guildAnnouncesURI = "/guilds/{guild_id}/announces"
	guildAnnounceURI  = "/guilds/{guild_id}/announces/{message_id}"

	schedulesURI uri = "/channels/{channel_id}/schedules"
	scheduleURI  uri = "/channels/{channel_id}/schedules/{schedule_id}"

	apiPermissionURI       uri = "/guilds/{guild_id}/api_permission"
	apiPermissionDemandURI uri = "/guilds/{guild_id}/api_permission/demand"

	pinsURI = "/channels/{channel_id}/pins"
	pinURI  = "/channels/{channel_id}/pins/{message_id}"

	messageReactionURI uri = "/channels/{channel_id}/messages/{message_id}/reactions/{emoji_type}/{emoji_id}"

	interactionsURI = "/interactions/{interaction_id}"

	httpSessionsURI uri = "/gateway/webhook/sessions"
	httpSessionURI  uri = "/gateway/webhook/sessions/{session_id}"

	messageSettingURI uri = "/guilds/{guild_id}/message/setting"

	voiceChannelMembersURI uri = "/channels/{channel_id}/voice/members"

	settingGuideURI   uri = "/channels/{channel_id}/settingguide"
	dmSettingGuideURI uri = "/dms/{guild_id}/settingguide"

	menuURI        uri = "/v2/menu"                     // 查询/修改全局自定义菜单
	panelsURI      uri = "/v2/panels"                   // 查询/创建指令面板
	panelURI       uri = "/v2/panels/{panel_id}"        // 查询/修改/删除指令面板
	panelTargetURI uri = "/v2/panels/{panel_id}/target" // 修改指令面板关联对象

	mediaImagesURI        uri = "/v2/media-images"                   // 素材列表
	mediaImagePrepareURI  uri = "/v2/media-images/prepare"           // 素材上传-申请
	mediaImageCompleteURI uri = "/v2/media-images/complete"          // 素材上传-完成
	mediaImageDeleteURI   uri = "/v2/media-images/{image_id}/delete" // 删除素材
)

// getURL 获取接口地址，会处理沙箱环境判断
func (o *openAPI) getURL(endpoint uri) string {
	d := constant.APIDomain
	if o.sandbox {
		d = constant.SandBoxAPIDomain
	}
	return fmt.Sprintf("%s%s", d, endpoint)
}
