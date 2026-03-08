# MMX High-Level Design (HLD)

## 1. 文档范围

本设计文档基于当前仓库实现（`main.go` + `internal/*` + `worker/*`）整理，重点覆盖：

- 核心媒体服务架构（基于 MediaMTX 的协议网关与 Path/Stream 模型）
- WebRTC Simulcast/ABR 设计与执行路径
- Origin/Edge 回源模型
- 转发、录制、控制面与运维接口

不覆盖 UI 细节和业务后台（仅描述与本仓库直接耦合接口）。

## 2. 设计目标

- 低延迟多协议接入与分发：RTSP / RTMP / SRT / WebRTC / HLS
- 以 Path 为中心管理发布/订阅、按需拉流、回源、录制与转发
- 在 WebRTC 读链路支持 Simulcast + ABR（自动/手动层切换）
- 兼容单二进制部署，同时内置 worker 任务能力

## 3. 总体架构

```text
Publisher -> Protocol Server -> PathManager -> Path -> Stream -> Readers
                                         |        |       |
                                         |        |       +-> Recorder
                                         |        +-> Forwarder(SRT/WebRTC)
                                         +-> StaticSource / OnDemand / OriginPull

WebRTC Read Session
  -> PeerConnection
  -> (optional) TrackSwitcher
  -> ABR control:
     1) WHEP PATCH(SDP b=AS) 重协商
     2) WebSocket SELECT_LAYER 手动切层
```

## 4. 关键模块

- 进程入口与装配：`main.go`、`internal/core/core.go`
- Path 生命周期与媒体主链路：`internal/core/path.go`
- WebRTC 服务：`internal/servers/webrtc/server.go`、`internal/servers/webrtc/http_server.go`、`internal/servers/webrtc/session.go`
- Track 切换与连续性：`internal/protocols/webrtc/track_switcher.go`
- WebSocket 协议模型：`internal/protocols/websocket/simulcast_control.go`
- ABR 独立 WS 服务：`internal/protocols/websocket/abr_server.go`
- Forward 转发：`internal/forwarder/manager.go`
- Worker 侧任务与上报：`worker/tasks/*`、`worker/services/*`

## 5. 核心运行流程

1. 启动阶段
- `main.go` 启动 `core.New()` 创建协议服务器与 PathManager。
- 同时按参数启用 worker 子系统（ws client、ingest、uploader、monitor、rest api）。

2. 发布流程
- Publisher 经任一协议进入对应 server（如 RTSP/RTMP/SRT/WebRTC）。
- `PathManager.AddPublisher()` 将源绑定到 Path，构建 `stream.Stream`。
- Path 变为 ready 后可触发录制、转发与 Hook。

3. 播放流程
- Reader 经协议 server 请求 Path。
- Path 已 ready：直接加入 reader；否则按配置触发 on-demand 拉起或回源。
- 回源场景可通过 `origNode` 配置按需 SRT 拉取。

4. WebRTC ABR 读流程
- WHEP `POST` 建立会话，服务端返回 session `Location`（含 secret）。
- 客户端通过 `PATCH application/sdp` 发送带 `b=AS` 的 offer 做重协商。
- 服务端解析带宽并选择目标轨道；有 `TrackSwitcher` 时做真实 RTP 切换。

## 6. ABR/Simulcast 设计要点

- ABR 开关来自全局配置：`webrtcABREnable`。
- ABR 打开时，读会话优先使用 `TrackSwitcher` 从多视频轨 + 音频轨构建可切换轨道集。
- TrackSwitcher 保证切轨时 RTP 序号/时间戳连续，并等待关键帧以减少花屏。
- 在极低带宽（实现中 `< 350kbps`）可切到 audio-only 轨道。
- 会话 API 增加 `simulcastBandwidthLimit` 便于观测当前 b=AS 约束。

## 7. 配置与部署拓扑

支持 Origin/Edge 分层部署（示例配置）：

- Origin 示例：`bin/conf/mmx_orig.yml`
- Edge 示例：`bin/conf/mmx_edge.yml`、`bin/conf/mmx_edge2.yml`

典型模式：

1. Origin 接收推流并对外提供多协议。
2. Edge 在本地无流时按 `origNode` 配置自动 SRT 回源。
3. Edge 开启 WebRTC ABR，为终端提供弱网自适应播放。

## 8. 运维与可观测性

- API：可列出/查询/kick WebRTC 会话（`internal/api/api_webrtc.go`）
- Metrics/PPROF：由 `core` 按配置启用
- Worker 侧监控、告警、上传与节点注册能力在 `worker/*` 中实现

## 9. 已知实现约束

- ABR 控制存在两条实现路径（独立 ABRServer 与 HTTP 内嵌 WS 处理），默认配置更偏向独立 ABRServer。
- 无 TrackSwitcher 时，切层可能退化为“仅状态更新，不做真实 RTP 切换”。
- 轨道元信息在部分场景使用默认值或推断值（尤其 origin 非 simulcast 单轨场景）。

