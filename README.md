
```md
# mmx

**mmx** 是一个基于 [mediamtx](https://github.com/bluenviron/mediamtx) 的实时音视频服务器扩展项目，  
目标是在保持 mediamtx 稳定、轻量特性的基础上，增强 **WebRTC Forwarding** 与 **Simulcast** 能力，  
以更好地支持弱网环境、多分辨率分发和多终端实时播放。

---

## ✨ 项目目标（Project Goals）

mmx 旨在解决以下问题：

- WebRTC 推流后，**无法灵活 forward 到多个 WebRTC / 协议端点**
- 单一路 WebRTC 流，**难以同时满足不同网络条件的客户端**
- 在弱网、移动网络（如 4G / 5G）环境下，播放端体验不稳定
- 希望在 **不引入复杂 SFU 架构** 的前提下，实现可控的多码率分发

### mmx 的核心目标：

- ✅ 在 mediamtx 基础上增加 **WebRTC Forward（无延迟转发）能力**
- ✅ 支持 **WebRTC Simulcast（多分辨率 / 多码率）**
- ✅ 与现有 mediamtx 协议栈（RTSP / SRT / RTMP / WebRTC）保持兼容
- ✅ 保持部署简单、资源占用可控
- ✅ 面向实时直播、弱网播放、低延迟场景

---

## 🧠 核心功能规划（Planned Features）

### 1. WebRTC zero-latency Forward

- WebRTC In → WebRTC Out
- 单路输入，多路 WebRTC 订阅
- Forward 到：
  - 不同房间 / 不同 Path
  - 不同域名 / 不同 ICE / 不同 SDP
- Forward 过程不重新采集、不重复解码（尽量复用 pipeline）

### 2. WebRTC Simulcast

- 支持 WebRTC Simulcast 编码（多分辨率 / 多码率）
- 典型层级示例：
  - 1080p @ 高码率
  - 720p @ 中码率
  - 360p @ 低码率
- 播放端可根据网络条件自动选择合适层
- 为弱网 / 移动端提供更稳定体验

### 3. Forward + Simulcast 组合能力

- Forward 时可选择：
  - 单一 simulcast 层
  - 动态切换 simulcast 层
- 为不同下游客户端提供差异化输出

---

## 🏗️ 架构设计思路（High-Level Architecture）

```

```
       ┌─────────────┐
       │  WebRTC In  │
       └──────┬──────┘
              │
      ┌───────▼────────┐
      │  mmx Core      │
      │  (mediamtx)   │
      ├───────────────┤
      │ Simulcast     │
      │ Forward Logic │
      └───────┬───────┘
              │
 ┌────────────┼─────────────┐
 │            │             │
```

┌────▼────┐  ┌────▼────┐  ┌─────▼─────┐
│ WebRTC  │  │ WebRTC  │  │ WebRTC    │
│ Client  │  │ Client  │  │ Client    │
│ (Low)   │  │ (Mid)   │  │ (High)    │
└─────────┘  └─────────┘  └───────────┘

```

---

## 🔧 技术基础（Technology Stack）

- **Base:** mediamtx
- **Language:** Go
- **Protocols:**
  - WebRTC
  - RTSP
  - RTMP
  - SRT
- **Media:**
  - H.264 / H.265 (HEVC)
  - Opus / AAC
- **Focus Areas:**
  - RTP / RTCP
  - SDP / ICE
  - Simulcast / RID / SSRC
  - 弱网适配

---

## 🚧 当前状态（Project Status）

- [x] 项目初始化
- [x] 基于 mediamtx 建立独立仓库（Mirror + Upstream）
- [ ] WebRTC Forward 设计
- [ ] Simulcast Pipeline 设计
- [ ] WebRTC Forward MVP
- [ ] Simulcast MVP
- [ ] 弱网测试（SRT / WebRTC）
- [ ] 性能与稳定性测试

> ⚠️ 当前处于 **早期开发阶段（Early Development）**，接口和实现细节可能发生变化。

---

## 📦 使用场景（Use Cases）

- 🎥 实时直播（低延迟）
- 📱 移动端 / 弱网播放
- 🌍 海外网络环境（高丢包、高 RTT）
- 🎮 游戏直播 / 虚拟直播
- 🧪 WebRTC 技术实验与研究

---

## 📜 License & Credits

本项目基于以下开源项目构建：

- **mediamtx**
  - https://github.com/bluenviron/mediamtx
  - License: MIT

mmx 在遵循原项目 License 的前提下，进行了功能扩展与工程化改造。

---

## 🤝 Contributing

当前阶段以核心功能开发为主，  
后续将逐步开放：

- 设计讨论
- Issue / Feature Request
- Pull Request

欢迎对 WebRTC / 实时音视频 / mediamtx 有兴趣的开发者交流。

---

## 📫 联系方式

- Issue
- Discussion
- Email
```

---
