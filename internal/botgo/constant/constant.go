// Package constant 集中定义 SDK 用到的常量（如接口域名、路径等）。
package constant

// HeaderTraceID 机器人openapi返回的链路追踪ID
const HeaderTraceID = "X-Tps-trace-ID"

// APIDomain api domain。
// 官方 2026-08-10 起所有接口调用域名统一为 api.bot.qq.com（原 api.sgroup.qq.com）。
var APIDomain = "https://api.bot.qq.com"

// SandBoxAPIDomain sandbox domain。
// 新版文档未给出沙箱域名，暂沿用旧值。
var SandBoxAPIDomain = "https://sandbox.api.sgroup.qq.com"

// TokenDomain token domain。
// 新版文档获取 AccessToken 地址为 https://api.bot.qq.com/app/getAppAccessToken（原 bots.qq.com）。
var TokenDomain = "https://api.bot.qq.com"
