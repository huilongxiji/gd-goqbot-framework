package v1

import (
	"context"
	"net/http"
	"strconv"

	"goqbot/internal/botgo/dto"
)

// GetMenu 查询全局自定义菜单
func (o *openAPI) GetMenu(ctx context.Context) (*dto.MenuGetResp, error) {
	resp, err := baseRequest(ctx,
		o.request(ctx).SetResult(dto.MenuGetResp{}),
		http.MethodGet, o.getURL(menuURI))
	if err != nil {
		return nil, err
	}
	return resp.Result().(*dto.MenuGetResp), nil
}

// SetMenu 修改全局自定义菜单（整体覆盖）
func (o *openAPI) SetMenu(ctx context.Context, req *dto.MenuPutReq) (*dto.VersionResp, error) {
	resp, err := baseRequest(ctx,
		o.request(ctx).
			SetResult(dto.VersionResp{}).
			SetBody(req),
		http.MethodPut, o.getURL(menuURI))
	if err != nil {
		return nil, err
	}
	return resp.Result().(*dto.VersionResp), nil
}

// ListCommandPanels 查询指令面板列表
func (o *openAPI) ListCommandPanels(ctx context.Context, pager *dto.PanelListPager) (*dto.PanelListResp, error) {
	req := o.request(ctx).SetResult(dto.PanelListResp{})
	if pager != nil {
		if pager.Scope != "" {
			req = req.SetQueryParam("scope", pager.Scope)
		}
		if pager.Limit > 0 {
			req = req.SetQueryParam("limit", strconv.Itoa(pager.Limit))
		}
		if pager.Cursor != "" {
			req = req.SetQueryParam("cursor", pager.Cursor)
		}
	}
	resp, err := baseRequest(ctx, req, http.MethodGet, o.getURL(panelsURI))
	if err != nil {
		return nil, err
	}
	return resp.Result().(*dto.PanelListResp), nil
}

// GetCommandPanel 查询指令面板详情
func (o *openAPI) GetCommandPanel(ctx context.Context, panelID string) (*dto.PanelDetail, error) {
	resp, err := baseRequest(ctx,
		o.request(ctx).
			SetResult(dto.PanelDetail{}).
			SetPathParam("panel_id", panelID),
		http.MethodGet, o.getURL(panelURI))
	if err != nil {
		return nil, err
	}
	return resp.Result().(*dto.PanelDetail), nil
}

// CreateCommandPanel 创建指令面板
func (o *openAPI) CreateCommandPanel(ctx context.Context, req *dto.PanelCreateReq) (*dto.PanelCreateResp, error) {
	resp, err := baseRequest(ctx,
		o.request(ctx).
			SetResult(dto.PanelCreateResp{}).
			SetBody(req),
		http.MethodPost, o.getURL(panelsURI))
	if err != nil {
		return nil, err
	}
	return resp.Result().(*dto.PanelCreateResp), nil
}

// SetCommandPanel 修改指令面板（整体覆盖）
func (o *openAPI) SetCommandPanel(ctx context.Context, panelID string, req *dto.PanelPutReq) (*dto.VersionResp, error) {
	resp, err := baseRequest(ctx,
		o.request(ctx).
			SetResult(dto.VersionResp{}).
			SetPathParam("panel_id", panelID).
			SetBody(req),
		http.MethodPut, o.getURL(panelURI))
	if err != nil {
		return nil, err
	}
	return resp.Result().(*dto.VersionResp), nil
}

// DeleteCommandPanel 删除指令面板
func (o *openAPI) DeleteCommandPanel(ctx context.Context, panelID string) error {
	_, err := baseRequest(ctx,
		o.request(ctx).SetPathParam("panel_id", panelID),
		http.MethodDelete, o.getURL(panelURI))
	return err
}

// SetCommandPanelTarget 修改指令面板关联对象（仅 specific 面板）
func (o *openAPI) SetCommandPanelTarget(ctx context.Context, panelID string, req *dto.PanelTargetReq) error {
	_, err := baseRequest(ctx,
		o.request(ctx).
			SetPathParam("panel_id", panelID).
			SetBody(req),
		http.MethodPut, o.getURL(panelTargetURI))
	return err
}
