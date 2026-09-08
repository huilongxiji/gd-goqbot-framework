package v1

import (
	"context"
	"net/http"
	"strconv"

	"goqbot/internal/botgo/dto"
)

// GroupInfo 获取群基本信息（白名单，非白名单返回 11253）
func (o *openAPI) GroupInfo(ctx context.Context, groupID string) (*dto.GroupInfo, error) {
	resp, err := baseRequest(ctx,
		o.request(ctx).
			SetResult(dto.GroupInfo{}).
			SetPathParam("group_id", groupID),
		http.MethodGet, o.getURL(groupInfoURI))
	if err != nil {
		return nil, err
	}
	return resp.Result().(*dto.GroupInfo), nil
}

// GroupBotState 获取机器人群内状态（白名单，非白名单返回 11253）
func (o *openAPI) GroupBotState(ctx context.Context, groupID string) (*dto.GroupBotState, error) {
	resp, err := baseRequest(ctx,
		o.request(ctx).
			SetResult(dto.GroupBotState{}).
			SetPathParam("group_id", groupID),
		http.MethodGet, o.getURL(groupBotStateURI))
	if err != nil {
		return nil, err
	}
	return resp.Result().(*dto.GroupBotState), nil
}

// GroupMembers 获取群成员列表（白名单；每次最多 30 条，cursor 分页）
func (o *openAPI) GroupMembers(ctx context.Context, groupID string, pager *dto.GroupMemberListPager) (*dto.GroupMemberList, error) {
	req := o.request(ctx).
		SetResult(dto.GroupMemberList{}).
		SetPathParam("group_id", groupID)
	if pager != nil && pager.Cursor != "" {
		req = req.SetQueryParam("cursor", pager.Cursor)
	}
	resp, err := baseRequest(ctx, req, http.MethodGet, o.getURL(groupMembersURI))
	if err != nil {
		return nil, err
	}
	return resp.Result().(*dto.GroupMemberList), nil
}

// GroupMember 获取指定群成员信息（白名单，非白名单返回 11253）
func (o *openAPI) GroupMember(ctx context.Context, groupID, memberID string) (*dto.GroupMember, error) {
	resp, err := baseRequest(ctx,
		o.request(ctx).
			SetResult(dto.GroupMember{}).
			SetPathParam("group_id", groupID).
			SetPathParam("member_id", memberID),
		http.MethodGet, o.getURL(groupMemberURI))
	if err != nil {
		return nil, err
	}
	return resp.Result().(*dto.GroupMember), nil
}

// GroupJoinRequestList 拉取入群申请列表（需群管理员）
func (o *openAPI) GroupJoinRequestList(ctx context.Context, groupID string, pager *dto.JoinRequestListPager) (*dto.JoinRequestList, error) {
	req := o.request(ctx).
		SetResult(dto.JoinRequestList{}).
		SetPathParam("group_id", groupID)
	if pager != nil {
		if pager.Cursor != "" {
			req = req.SetQueryParam("cursor", pager.Cursor)
		}
		if pager.Limit > 0 {
			req = req.SetQueryParam("limit", strconv.Itoa(pager.Limit))
		}
	}
	resp, err := baseRequest(ctx, req, http.MethodGet, o.getURL(groupJoinRequestListURI))
	if err != nil {
		return nil, err
	}
	return resp.Result().(*dto.JoinRequestList), nil
}

// ApprovalGroupJoinRequest 审批入群申请（需群管理员）
func (o *openAPI) ApprovalGroupJoinRequest(ctx context.Context, groupID, memberID string, req *dto.ApprovalJoinRequestReq) error {
	_, err := baseRequest(ctx,
		o.request(ctx).
			SetPathParam("group_id", groupID).
			SetPathParam("member_id", memberID).
			SetBody(req),
		http.MethodPost, o.getURL(groupApprovalJoinRequestURI))
	return err
}

// GroupRestrictChatSetting 查询群禁言状态（需群管理员）
func (o *openAPI) GroupRestrictChatSetting(ctx context.Context, groupID string) (*dto.RestrictChatSetting, error) {
	resp, err := baseRequest(ctx,
		o.request(ctx).
			SetResult(dto.RestrictChatSetting{}).
			SetPathParam("group_id", groupID),
		http.MethodGet, o.getURL(groupRestrictChatURI))
	if err != nil {
		return nil, err
	}
	return resp.Result().(*dto.RestrictChatSetting), nil
}

// SetGroupRestrictChat 设置群成员禁言（需群管理员；单次最多 10 人）
func (o *openAPI) SetGroupRestrictChat(ctx context.Context, groupID string, req *dto.SetRestrictChatReq) error {
	_, err := baseRequest(ctx,
		o.request(ctx).
			SetPathParam("group_id", groupID).
			SetBody(req),
		http.MethodPost, o.getURL(groupRestrictChatURI))
	return err
}
