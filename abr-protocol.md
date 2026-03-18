# MMX ABR Protocol (Implementation-Aligned)

## 1. 目标与范围

本文档描述 mmx 当前实现中的 ABR 协议交互，包括：

- WHEP 会话建立与 SDP 重协商（`POST`/`PATCH`）
- WebSocket 控制通道（轨道列表下发、手动切层、心跳）
- 服务端轨道切换与带宽映射逻辑

## 2. 参与方

- ABR Client：播放器（`mmxplayer/*`）
- WebRTC/WHEP Server：`internal/servers/webrtc/*`
- ABR WS Server：`internal/protocols/websocket/abr_server.go`（独立）或 `ws_conn.go`（会话内）
- Session/Track 控制核心：`internal/servers/webrtc/session.go` + `internal/protocols/webrtc/track_switcher.go`

## 3. 会话建立（WHEP）

1. Client -> `POST /{path}/whep`
- `Content-Type: application/sdp`
- Body: Offer SDP

2. Server -> `201 Created`
- Body: Answer SDP
- Header:
- `Location: /{path}/whep/{secret}`
- `ID: {session_uuid}`
- `Accept-Patch: application/trickle-ice-sdpfrag, application/sdp`

说明：

- `secret` 用于后续 PATCH/DELETE
- `session_uuid` 常用于 WS `session_id` 参数

## 4. ABR 重协商（WHEP PATCH）

1. Client 对 `Location` 发起：
- `PATCH /{path}/whep/{secret}`
- `Content-Type: application/sdp`
- Body: 新 Offer SDP（包含 `b=AS:<kbps>`）

2. Server 行为：
- 解析 SDP，提取带宽约束
- 根据带宽选择目标 track（可触发音频保活模式）
- 对 peer connection 执行 renegotiation：
- `SetRemoteDescription` -> `CreateAnswer` -> `SetLocalDescription`

3. Server 响应：
- `200 OK` + Answer SDP（当前实现主路径）
- 或 `204 No Content`（兼容分支）

## 5. Trickle ICE（并行补充）

- `PATCH /{path}/whep/{secret}`
- `Content-Type: application/trickle-ice-sdpfrag`
- Body: ICE fragment
- 成功返回 `204 No Content`

## 6. WebSocket ABR 控制协议

## 6.1 连接方式

- 典型 URL：
- `ws://<host>:<port>/ws/control?session_id=<session_uuid>&path=<path>`

注：实现支持 `session_id` 或 `path` 查询参数用于轨道发现与控制路由。

## 6.2 消息类型

- `TRACKS_INFO`：服务端下发可选轨道及当前轨道
- `SELECT_LAYER`：客户端请求切到目标轨道
- `LAYER_SWITCHED`：服务端确认切换完成
- `PING` / `PONG`：心跳
- `ERROR`：错误回传

## 6.3 消息结构（存在两种兼容格式）

格式 A（WSMessage）：

```json
{
  "msg_id": "uuid",
  "type": "SELECT_LAYER",
  "timestamp": 1730000000,
  "payload": {
    "target_track_id": 2,
    "reason": "manual"
  }
}
```

格式 B（ABRMessage）：

```json
{
  "msg_id": "uuid",
  "type": "SELECT_LAYER",
  "data": {
    "target_track_id": 2,
    "reason": "manual"
  }
}
```

客户端通常兼容 `payload` 与 `data` 两种字段。

## 6.4 TRACKS_INFO 示例

```json
{
  "type": "TRACKS_INFO",
  "payload": {
    "active_track_id": 0,
    "tracks": [
      { "id": 0, "type": "video", "codec": "h264", "label": "High", "bitrate": 2000000, "width": 1920, "height": 1080 },
      { "id": 1, "type": "video", "codec": "h264", "label": "Medium", "bitrate": 1000000, "width": 1280, "height": 720 },
      { "id": 2, "type": "video", "codec": "h264", "label": "Low", "bitrate": 400000, "width": 960, "height": 540 },
      { "id": 3, "type": "audio", "codec": "opus", "label": "Audio", "bitrate": 128000 }
    ]
  }
}
```

## 7. 服务端轨道选择逻辑

1. 带宽驱动（自动）
- 从 SDP 尝试提取 `b=AS`（kbps）
- 根据可用 track 选“不超过带宽的最高视频轨”
- 若带宽极低（实现阈值 `<350kbps`），优先切到 audio-only

2. 手动切层（WS）
- 收到 `SELECT_LAYER.target_track_id`
- 调用 `session.SwitchVideoTrack(targetID)`

3. 实际切换机制
- 有 TrackSwitcher：执行真实 RTP 源切换，保持 seq/timestamp 连续，并关键帧对齐
- 无 TrackSwitcher：可能仅更新会话状态，不发生实际媒体轨切换

## 8. 错误与状态码

- `400`：无效 Content-Type、无效 SDP/参数
- `404`：session 不存在或 secret/path 不匹配
- `500`：内部切换/重协商失败
- WS `ERROR` 负载包含 `code` 与 `message`（或兼容 `error` 字段）

## 9. 配置项（全局）

来自 `internal/conf/conf.go`：

- `webrtcABREnable`
- `webrtcABRWSPath`
- `webrtcABRMinBitrate`
- `webrtcABRMaxBitrate`
- `webrtcABRDefaultQuality`
- `webrtcABRSwitchThreshold`

建议与前端逻辑保持一致：切层冷却时间、自动/手动优先级、音频兜底策略。

