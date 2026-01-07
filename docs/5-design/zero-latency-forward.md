# 无延迟转发功能设计文档

## 1. 概述

### 1.1 目标
在 mediamtx 基础上实现无延迟转发功能，支持将接收到的流直接转发到其他服务器，无需使用 FFmpeg，避免重新编码带来的延迟。

### 1.2 核心原则
- **零延迟**：直接复用 RTP 包，不重新编码
- **多协议支持**：支持 WebRTC、RTSP、SRT、RTMP 等协议
- **高可用性**：支持断线重连、错误恢复
- **配置灵活**：支持多个转发目标、条件转发

## 2. 架构设计

### 2.1 整体架构

```
┌─────────────┐
│   Publisher │  (WebRTC/RTSP/SRT/RTMP)
└──────┬──────┘
       │
       ▼
┌─────────────────┐
│  Stream (Core)  │
└──────┬──────────┘
       │
       ├─────────────────────────────────┐
       │                                 │
       ▼                                 ▼
┌──────────────┐              ┌──────────────────┐
│   Readers    │              │   Forwarders     │
│  (Clients)   │              │  (Zero-Latency)  │
└──────────────┘              └────────┬─────────┘
                                       │
                    ┌──────────────────┼──────────────────┐
                    │                  │                  │
                    ▼                  ▼                  ▼
            ┌─────────────┐    ┌─────────────┐    ┌─────────────┐
            │  WebRTC     │    │    RTSP      │    │    SRT       │
            │  Forwarder  │    │  Forwarder   │    │  Forwarder   │
            └─────────────┘    └─────────────┘    └─────────────┘
                    │                  │                  │
                    ▼                  ▼                  ▼
            ┌─────────────┐    ┌─────────────┐    ┌─────────────┐
            │  Remote     │    │   Remote    │    │   Remote    │
            │  Server     │    │   Server    │    │   Server    │
            └─────────────┘    └─────────────┘    └─────────────┘
```

### 2.2 核心组件

#### 2.2.1 Forwarder 管理器
- **位置**: `internal/forwarder/manager.go`
- **职责**:
  - 管理所有转发器实例
  - 根据配置创建/销毁转发器
  - 处理转发器生命周期

#### 2.2.2 协议转发器接口
- **位置**: `internal/forwarder/forwarder.go`
- **接口定义**:
```go
type Forwarder interface {
    Start() error
    Stop()
    IsRunning() bool
    GetStats() Stats
}
```

#### 2.2.3 各协议转发器实现
- **WebRTC Forwarder**: `internal/forwarder/webrtc/forwarder.go`
- **RTSP Forwarder**: `internal/forwarder/rtsp/forwarder.go`
- **SRT Forwarder**: `internal/forwarder/srt/forwarder.go`
- **RTMP Forwarder**: `internal/forwarder/rtmp/forwarder.go`

## 3. 数据流设计

### 3.1 数据流转过程

```
Stream.WriteRTPPacket()
    │
    ├─> Stream.medias[media].formats[format].writeRTPPacket()
    │       │
    │       ├─> RTSP Server Stream (本地 RTSP 输出)
    │       ├─> Readers (本地客户端)
    │       └─> Forwarders (转发器) ← 新增
    │               │
    │               ├─> WebRTC Forwarder
    │               │       └─> 直接发送 RTP 包到远程 WebRTC 服务器
    │               │
    │               ├─> RTSP Forwarder
    │               │       └─> 使用 gortsplib.Client 发送 RTP 包
    │               │
    │               ├─> SRT Forwarder
    │               │       └─> 封装为 MPEG-TS 后通过 SRT 发送
    │               │
    │               └─> RTMP Forwarder
    │                       └─> 转换为 FLV 格式后通过 RTMP 发送
```

### 3.2 关键实现点

#### 3.2.1 复用 Stream.Reader 机制
转发器可以作为 Stream 的 Reader，通过 `OnData` 回调接收数据：

```go
reader := &stream.Reader{Parent: forwarder}
reader.OnData(media, format, func(u *unit.Unit) error {
    // 直接使用 RTP 包，无需重新编码
    for _, pkt := range u.RTPPackets {
        forwarder.sendRTP(pkt)
    }
    return nil
})
stream.AddReader(reader)
```

#### 3.2.2 协议特定处理

**WebRTC Forwarder**:
- 使用 `pion/webrtc` 库建立 PeerConnection
- 创建 OutgoingTrack，直接写入 RTP 包
- 复用现有的 `internal/protocols/webrtc/outgoing_track.go` 逻辑

**RTSP Forwarder**:
- 使用 `gortsplib.Client` 连接到远程 RTSP 服务器
- 调用 `StartRecording()` 开始推流
- 直接发送 RTP 包，无需重新编码

**SRT Forwarder**:
- 使用 SRT 库建立连接
- 将 RTP 包封装为 MPEG-TS 格式
- 通过 SRT 发送（需要复用现有的 MPEG-TS 封装逻辑）

**RTMP Forwarder**:
- 使用 RTMP 库建立连接
- 将 RTP 包转换为 FLV 格式
- 通过 RTMP 发送（需要复用现有的 RTMP 转换逻辑）

## 4. 配置设计

### 4.1 配置结构

在 `internal/conf/path.go` 中添加：

```go
type Path struct {
    // ... 现有字段 ...
    
    // Forwarding
    ForwardTargets []ForwardTarget `json:"forwardTargets"`
}

type ForwardTarget struct {
    // 目标 URL，支持以下格式：
    // - webrtc://host:port/path (WHEP)
    // - rtsp://host:port/path
    // - rtsps://host:port/path
    // - srt://host:port?streamid=publish:path
    // - rtmp://host:port/path
    // - rtmps://host:port/path
    URL string `json:"url"`
    
    // 是否启用
    Enable bool `json:"enable"`
    
    // 重连配置
    Reconnect        bool     `json:"reconnect"`         // 是否自动重连
    ReconnectDelay   Duration `json:"reconnectDelay"`     // 重连延迟
    MaxReconnectTime Duration `json:"maxReconnectTime"`  // 最大重连时间
    
    // 认证信息（如果需要）
    Username string `json:"username,omitempty"`
    Password string `json:"password,omitempty"`
    
    // 协议特定配置
    WebRTCConfig *ForwardWebRTCConfig `json:"webrtcConfig,omitempty"`
    RTSPConfig  *ForwardRTSPConfig   `json:"rtspConfig,omitempty"`
    SRTConfig   *ForwardSRTConfig    `json:"srtConfig,omitempty"`
    RTMPConfig  *ForwardRTMPConfig   `json:"rtmpConfig,omitempty"`
}

type ForwardWebRTCConfig struct {
    ICE Servers []string `json:"iceServers,omitempty"`
}

type ForwardRTSPConfig struct {
    Transport RTSPTransport `json:"transport"` // udp, tcp, automatic
}

type ForwardSRTConfig struct {
    Passphrase string `json:"passphrase,omitempty"`
    Latency    uint   `json:"latency"` // 毫秒
}

type ForwardRTMPConfig struct {
    // RTMP 特定配置
}
```

### 4.2 配置示例

```yaml
paths:
  mystream:
    # 转发到多个目标
    forwardTargets:
      # WebRTC 转发
      - url: webrtc://remote-server:8889/target-path/whep
        enable: yes
        reconnect: yes
        reconnectDelay: 2s
        webrtcConfig:
          iceServers:
            - stun:stun.l.google.com:19302
      
      # RTSP 转发
      - url: rtsp://remote-server:8554/target-path
        enable: yes
        reconnect: yes
        reconnectDelay: 2s
        username: user
        password: pass
        rtspConfig:
          transport: tcp
      
      # SRT 转发
      - url: srt://remote-server:8890?streamid=publish:target-path
        enable: yes
        reconnect: yes
        srtConfig:
          passphrase: mypassphrase
          latency: 120
      
      # RTMP 转发
      - url: rtmp://remote-server:1935/target-path
        enable: yes
        reconnect: yes
        username: user
        password: pass
```

## 5. 实现细节

### 5.1 Forwarder 管理器集成

在 `internal/core/path.go` 中：

```go
type path struct {
    // ... 现有字段 ...
    forwarderManager *forwarder.Manager
}

func (pa *path) initialize() {
    // ... 现有初始化代码 ...
    
    // 初始化转发器管理器
    pa.forwarderManager = forwarder.NewManager(
        pa.ctx,
        pa.conf.ForwardTargets,
        pa.stream,
        pa,
    )
}

func (pa *path) onStreamReady() {
    // ... 现有代码 ...
    
    // 启动转发器
    if pa.stream != nil {
        pa.forwarderManager.Start(pa.stream)
    }
}

func (pa *path) onStreamNotReady() {
    // ... 现有代码 ...
    
    // 停止转发器
    pa.forwarderManager.Stop()
}
```

### 5.2 WebRTC Forwarder 实现

```go
package webrtc

type Forwarder struct {
    url      string
    config   *ForwardWebRTCConfig
    pc       *webrtc.PeerConnection
    tracks  []*OutgoingTrack
    reader  *stream.Reader
    stream  *stream.Stream
    logger  logger.Writer
    ctx     context.Context
    ctxCancel context.CancelFunc
}

func (f *Forwarder) Start(stream *stream.Stream) error {
    f.stream = stream
    
    // 创建 WebRTC PeerConnection
    pc, err := f.createPeerConnection()
    if err != nil {
        return err
    }
    f.pc = pc
    
    // 创建 Reader 并注册回调
    f.reader = &stream.Reader{Parent: f}
    
    // 设置 tracks
    err = f.setupTracks(stream.Desc)
    if err != nil {
        return err
    }
    
    // 创建 Offer 并发送到远程服务器
    offer, err := pc.CreateOffer(nil)
    if err != nil {
        return err
    }
    
    err = pc.SetLocalDescription(offer)
    if err != nil {
        return err
    }
    
    // 发送 Offer 到远程服务器（WHEP）
    answer, err := f.sendOffer(offer)
    if err != nil {
        return err
    }
    
    err = pc.SetRemoteDescription(answer)
    if err != nil {
        return err
    }
    
    // 等待连接建立
    err = pc.WaitUntilConnected()
    if err != nil {
        return err
    }
    
    // 添加 Reader 到 Stream
    stream.AddReader(f.reader)
    
    return nil
}

func (f *Forwarder) setupTracks(desc *description.Session) error {
    for _, media := range desc.Medias {
        for _, format := range media.Formats {
            track, err := f.createOutgoingTrack(media, format)
            if err != nil {
                return err
            }
            f.tracks = append(f.tracks, track)
            
            // 注册数据回调
            f.reader.OnData(media, format, func(u *unit.Unit) error {
                for _, pkt := range u.RTPPackets {
                    track.WriteRTPWithNTP(pkt, u.NTP)
                }
                return nil
            })
        }
    }
    return nil
}
```

### 5.3 RTSP Forwarder 实现

```go
package rtsp

type Forwarder struct {
    url     string
    config  *ForwardRTSPConfig
    client  *gortsplib.Client
    reader  *stream.Reader
    stream  *stream.Stream
    logger  logger.Writer
    ctx     context.Context
    ctxCancel context.CancelFunc
}

func (f *Forwarder) Start(stream *stream.Stream) error {
    f.stream = stream
    
    // 解析 URL
    u, err := base.ParseURL(f.url)
    if err != nil {
        return err
    }
    
    // 创建 RTSP Client
    f.client = &gortsplib.Client{
        Protocol: f.config.Transport.Protocol,
        // ... 其他配置 ...
    }
    
    err = f.client.Start()
    if err != nil {
        return err
    }
    
    // 开始推流
    err = f.client.StartRecording(u, stream.Desc)
    if err != nil {
        return err
    }
    
    // 创建 Reader 并注册回调
    f.reader = &stream.Reader{Parent: f}
    
    for _, media := range stream.Desc.Medias {
        for _, format := range media.Formats {
            f.reader.OnData(media, format, func(u *unit.Unit) error {
                for _, pkt := range u.RTPPackets {
                    f.client.WritePacketRTPWithNTP(media, pkt, u.NTP)
                }
                return nil
            })
        }
    }
    
    // 添加 Reader 到 Stream
    stream.AddReader(f.reader)
    
    return nil
}
```

### 5.4 错误处理和重连

```go
func (f *Forwarder) runWithReconnect() {
    for {
        err := f.Start(f.stream)
        if err != nil {
            f.logger.Log(logger.Warn, "forwarder error: %v", err)
        }
        
        // 等待连接失败或上下文取消
        select {
        case <-f.pc.Failed():
            // 连接失败，等待重连延迟
            if f.config.Reconnect {
                time.Sleep(time.Duration(f.config.ReconnectDelay))
                continue
            }
            return
            
        case <-f.ctx.Done():
            return
        }
    }
}
```

## 6. 性能优化

### 6.1 零拷贝优化
- 直接复用 RTP 包，避免内存拷贝
- 使用引用计数管理 RTP 包生命周期

### 6.2 并发处理
- 每个转发器在独立的 goroutine 中运行
- 使用 channel 进行异步数据传输

### 6.3 缓冲管理
- 转发器使用独立的缓冲队列
- 避免转发失败影响本地播放

## 7. 测试策略

### 7.1 单元测试
- 测试各协议转发器的基本功能
- 测试错误处理和重连逻辑

### 7.2 集成测试
- 测试多协议转发
- 测试断线重连
- 测试性能指标（延迟、丢包率）

### 7.3 端到端测试
- 使用真实服务器测试转发功能
- 验证延迟指标（目标：< 50ms）

## 8. 实施计划

### Phase 1: 基础框架
1. 创建 Forwarder 接口和基础框架
2. 实现 Forwarder 管理器
3. 集成到 Path 生命周期

### Phase 2: RTSP 转发器
1. 实现 RTSP Forwarder（最简单）
2. 测试和优化

### Phase 3: WebRTC 转发器
1. 实现 WebRTC Forwarder
2. 支持 WHEP 协议
3. 测试和优化

### Phase 4: SRT/RTMP 转发器
1. 实现 SRT Forwarder
2. 实现 RTMP Forwarder
3. 测试和优化

### Phase 5: 完善功能
1. 错误处理和重连
2. 性能优化
3. 文档和示例

## 9. 注意事项

### 9.1 协议兼容性
- 不同协议的编码格式要求可能不同
- 需要处理编码格式转换（如 RTMP 需要 FLV 格式）

### 9.2 网络稳定性
- 实现断线重连机制
- 处理网络抖动和丢包

### 9.3 资源管理
- 及时释放连接资源
- 避免内存泄漏

### 9.4 配置验证
- 验证转发目标 URL 格式
- 验证认证信息

## 10. 未来扩展

### 10.1 条件转发
- 根据流质量、客户端数量等条件决定是否转发

### 10.2 负载均衡
- 支持多个转发目标之间的负载均衡

### 10.3 统计和监控
- 转发成功率、延迟统计
- 集成到现有的 metrics 系统

### 10.4 动态配置
- 支持通过 API 动态添加/删除转发目标

