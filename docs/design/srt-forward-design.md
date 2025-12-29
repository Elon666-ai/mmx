# SRT 无延迟转发功能设计文档

## 1. 概述

### 1.1 目标
实现 SRT 到 SRT 的无延迟转发功能，将接收到的流直接转发到其他 SRT 服务器，无需使用 FFmpeg。

### 1.2 核心原则
- **零延迟**：直接复用 MPEG-TS 封装逻辑，不重新编码
- **高可用性**：支持断线重连、错误恢复
- **配置灵活**：支持多个转发目标

## 2. 架构设计

### 2.1 整体架构

```
┌─────────────┐
│   Publisher │  (SRT/RTSP/WebRTC/RTMP)
└──────┬──────┘
       │
       ▼
┌─────────────────┐
│  Stream (Core)  │
└──────┬──────────┘
       │
       ├─────────────────┐
       │                 │
       ▼                 ▼
┌──────────────┐  ┌──────────────────┐
│   Readers    │  │  SRT Forwarder   │
│  (Clients)   │  │  (Zero-Latency)  │
└──────────────┘  └────────┬──────────┘
                           │
                           ▼
                  ┌─────────────┐
                  │   Remote    │
                  │ SRT Server  │
                  └─────────────┘
```

### 2.2 数据流设计

```
Stream.WriteUnit()
    │
    ├─> Stream.medias[media].formats[format].writeUnit()
    │       │
    │       ├─> RTSP Server Stream (本地 RTSP 输出)
    │       ├─> Readers (本地客户端)
    │       └─> SRT Forwarder (转发器) ← 新增
    │               │
    │               └─> 使用 mpegts.FromStream() 封装为 MPEG-TS
    │                       └─> 通过 SRT 连接发送到远程服务器
```

### 2.3 关键实现点

#### 2.3.1 复用现有 MPEG-TS 封装逻辑
mediamtx 已经实现了 `mpegts.FromStream()` 函数，可以将 Stream 转换为 MPEG-TS 格式。我们可以直接复用这个函数：

```go
// 在 internal/protocols/mpegts/from_stream.go 中已有实现
func FromStream(
    desc *description.Session,
    r *stream.Reader,
    bw *bufio.Writer,
    sconn srt.Conn,
    writeTimeout time.Duration,
) error
```

#### 2.3.2 SRT 客户端连接
使用 `github.com/datarhei/gosrt` 库建立 SRT 客户端连接：

```go
srtConf := srt.DefaultConfig()
address, err := srtConf.UnmarshalURL(targetURL)
err = srtConf.Validate()

sconn, err := srt.Dial("srt", address, srtConf)
```

## 3. 配置设计

### 3.1 配置结构

在 `internal/conf/path.go` 中添加：

```go
type Path struct {
    // ... 现有字段 ...
    
    // SRT Forwarding
    SRTForwardTargets []SRTForwardTarget `json:"srtForwardTargets"`
}

type SRTForwardTarget struct {
    // 目标 SRT URL，格式：srt://host:port?streamid=publish:path
    URL string `json:"url"`
    
    // 是否启用
    Enable bool `json:"enable"`
    
    // 重连配置
    Reconnect        bool     `json:"reconnect"`         // 是否自动重连
    ReconnectDelay   Duration `json:"reconnectDelay"`     // 重连延迟
    MaxReconnectTime Duration `json:"maxReconnectTime"`  // 最大重连时间
    
    // SRT 特定配置
    Passphrase string `json:"passphrase,omitempty"`  // SRT 密码
    Latency    uint   `json:"latency"`               // 延迟（毫秒），默认 120
    PacketSize uint   `json:"packetSize"`            // 包大小，默认 1316
}
```

### 3.2 配置示例

```yaml
paths:
  mystream:
    # SRT 转发配置
    srtForwardTargets:
      # 转发到远程 SRT 服务器
      - url: srt://remote-server:8890?streamid=publish:target-path
        enable: yes
        reconnect: yes
        reconnectDelay: 2s
        passphrase: mypassphrase
        latency: 120
        packetSize: 1316
      
      # 转发到另一个服务器
      - url: srt://another-server:8890?streamid=publish:another-path
        enable: yes
        reconnect: yes
        reconnectDelay: 2s
```

## 4. 实现细节

### 4.1 目录结构

```
internal/
├── forwarder/
│   ├── manager.go          # 转发器管理器
│   ├── forwarder.go        # 转发器接口定义
│   └── srt/
│       └── forwarder.go     # SRT 转发器实现
```

### 4.2 核心接口定义

```go
package forwarder

import (
    "context"
    "github.com/bluenviron/mediamtx/internal/stream"
    "github.com/bluenviron/mediamtx/internal/logger"
)

// Forwarder 是转发器的通用接口
type Forwarder interface {
    // Start 启动转发器
    Start(stream *stream.Stream) error
    
    // Stop 停止转发器
    Stop()
    
    // IsRunning 返回转发器是否正在运行
    IsRunning() bool
    
    // GetStats 返回统计信息
    GetStats() Stats
    
    // GetTarget 返回转发目标 URL
    GetTarget() string
}

// Stats 是转发器统计信息
type Stats struct {
    BytesSent     uint64
    PacketsSent   uint64
    PacketsLost   uint64
    LastError     error
    Connected     bool
    ReconnectCount uint64
}
```

### 4.3 SRT Forwarder 实现

```go
package srt

import (
    "bufio"
    "context"
    "fmt"
    "net/url"
    "sync"
    "sync/atomic"
    "time"
    
    srt "github.com/datarhei/gosrt"
    "github.com/bluenviron/gortsplib/v5/pkg/description"
    "github.com/bluenviron/mediamtx/internal/conf"
    "github.com/bluenviron/mediamtx/internal/forwarder"
    "github.com/bluenviron/mediamtx/internal/logger"
    "github.com/bluenviron/mediamtx/internal/protocols/mpegts"
    "github.com/bluenviron/mediamtx/internal/stream"
)

type Forwarder struct {
    url       string
    config    *conf.SRTForwardTarget
    stream    *stream.Stream
    reader    *stream.Reader
    sconn     srt.Conn
    logger    logger.Writer
    ctx       context.Context
    ctxCancel context.CancelFunc
    wg        sync.WaitGroup
    mutex     sync.RWMutex
    
    // 统计信息
    bytesSent     uint64
    packetsSent   uint64
    packetsLost   uint64
    lastError     error
    connected     bool
    reconnectCount uint64
    
    // SRT 配置
    writeTimeout time.Duration
    udpMaxPayloadSize int
}

func New(url string, config *conf.SRTForwardTarget, parent logger.Writer, writeTimeout time.Duration, udpMaxPayloadSize int) *Forwarder {
    ctx, ctxCancel := context.WithCancel(context.Background())
    
    return &Forwarder{
        url:              url,
        config:           config,
        logger:           parent,
        ctx:              ctx,
        ctxCancel:        ctxCancel,
        writeTimeout:     writeTimeout,
        udpMaxPayloadSize: udpMaxPayloadSize,
    }
}

func (f *Forwarder) Start(strm *stream.Stream) error {
    f.mutex.Lock()
    defer f.mutex.Unlock()
    
    if f.stream != nil {
        return fmt.Errorf("forwarder already started")
    }
    
    f.stream = strm
    f.wg.Add(1)
    go f.run()
    
    return nil
}

func (f *Forwarder) Stop() {
    f.ctxCancel()
    f.wg.Wait()
    
    f.mutex.Lock()
    if f.reader != nil && f.stream != nil {
        f.stream.RemoveReader(f.reader)
    }
    if f.sconn != nil {
        f.sconn.Close()
    }
    f.stream = nil
    f.reader = nil
    f.sconn = nil
    f.mutex.Unlock()
}

func (f *Forwarder) IsRunning() bool {
    f.mutex.RLock()
    defer f.mutex.RUnlock()
    return f.stream != nil
}

func (f *Forwarder) GetStats() forwarder.Stats {
    f.mutex.RLock()
    defer f.mutex.RUnlock()
    
    return forwarder.Stats{
        BytesSent:     atomic.LoadUint64(&f.bytesSent),
        PacketsSent:   atomic.LoadUint64(&f.packetsSent),
        PacketsLost:   atomic.LoadUint64(&f.packetsLost),
        LastError:     f.lastError,
        Connected:     f.connected,
        ReconnectCount: atomic.LoadUint64(&f.reconnectCount),
    }
}

func (f *Forwarder) GetTarget() string {
    return f.url
}

func (f *Forwarder) run() {
    defer f.wg.Done()
    
    for {
        select {
        case <-f.ctx.Done():
            return
        default:
            err := f.runInner()
            if err != nil {
                f.mutex.Lock()
                f.lastError = err
                f.connected = false
                f.mutex.Unlock()
                
                f.logger.Log(logger.Warn, "SRT forwarder error: %v", err)
                
                if f.config.Reconnect {
                    atomic.AddUint64(&f.reconnectCount, 1)
                    time.Sleep(time.Duration(f.config.ReconnectDelay))
                    continue
                }
                return
            }
        }
    }
}

func (f *Forwarder) runInner() error {
    // 解析 URL
    srtConf := srt.DefaultConfig()
    address, err := srtConf.UnmarshalURL(f.url)
    if err != nil {
        return fmt.Errorf("invalid SRT URL: %w", err)
    }
    
    // 设置 SRT 配置
    if f.config.Passphrase != "" {
        srtConf.Passphrase = f.config.Passphrase
    }
    if f.config.Latency > 0 {
        srtConf.Latency = time.Duration(f.config.Latency) * time.Millisecond
    } else {
        srtConf.Latency = 120 * time.Millisecond // 默认值
    }
    if f.config.PacketSize > 0 {
        srtConf.PayloadSize = f.config.PacketSize
    } else {
        srtConf.PayloadSize = 1316 // 默认值
    }
    
    err = srtConf.Validate()
    if err != nil {
        return fmt.Errorf("invalid SRT config: %w", err)
    }
    
    // 建立 SRT 连接
    sconn, err := srt.Dial("srt", address, srtConf)
    if err != nil {
        return fmt.Errorf("failed to connect: %w", err)
    }
    
    f.mutex.Lock()
    f.sconn = sconn
    f.connected = true
    f.mutex.Unlock()
    
    defer func() {
        sconn.Close()
        f.mutex.Lock()
        f.sconn = nil
        f.connected = false
        f.mutex.Unlock()
    }()
    
    // 创建 buffered writer
    maxPayloadSize := f.srtMaxPayloadSize()
    bw := bufio.NewWriterSize(sconn, maxPayloadSize)
    
    // 创建 Reader
    f.reader = &stream.Reader{Parent: f}
    
    // 使用 mpegts.FromStream 将 Stream 转换为 MPEG-TS 并通过 SRT 发送
    err = mpegts.FromStream(f.stream.Desc, f.reader, bw, sconn, f.writeTimeout)
    if err != nil {
        return fmt.Errorf("failed to setup MPEG-TS writer: %w", err)
    }
    
    // 添加 Reader 到 Stream
    f.stream.AddReader(f.reader)
    defer f.stream.RemoveReader(f.reader)
    
    // 等待连接失败或上下文取消
    done := make(chan error, 1)
    go func() {
        // 监控连接状态
        for {
            select {
            case <-f.ctx.Done():
                done <- nil
                return
            default:
                // 检查连接状态
                stats := sconn.Stats()
                if stats.Instantaneous.MsRTT == 0 && stats.Instantaneous.MsRTT == 0 {
                    // 连接可能已断开
                    done <- fmt.Errorf("SRT connection lost")
                    return
                }
                time.Sleep(1 * time.Second)
            }
        }
    }()
    
    // 等待错误或上下文取消
    select {
    case err := <-done:
        return err
    case <-f.ctx.Done():
        return nil
    }
}

func (f *Forwarder) srtMaxPayloadSize() int {
    // 计算最大 payload size
    // SRT header = 16 bytes, MPEG-TS packet = 188 bytes
    return ((f.udpMaxPayloadSize - 16) / 188) * 188
}
```

### 4.4 Manager 实现

```go
package forwarder

import (
    "context"
    "github.com/bluenviron/mediamtx/internal/conf"
    "github.com/bluenviron/mediamtx/internal/forwarder/srt"
    "github.com/bluenviron/mediamtx/internal/logger"
    "github.com/bluenviron/mediamtx/internal/stream"
    "time"
)

type Manager struct {
    forwarders []Forwarder
    stream     *stream.Stream
    logger     logger.Writer
    ctx        context.Context
    ctxCancel  context.CancelFunc
    writeTimeout time.Duration
    udpMaxPayloadSize int
}

func NewManager(
    ctx context.Context,
    targets []conf.SRTForwardTarget,
    stream *stream.Stream,
    parent logger.Writer,
    writeTimeout time.Duration,
    udpMaxPayloadSize int,
) *Manager {
    ctx, ctxCancel := context.WithCancel(ctx)
    
    m := &Manager{
        stream:            stream,
        logger:            parent,
        ctx:               ctx,
        ctxCancel:         ctxCancel,
        writeTimeout:      writeTimeout,
        udpMaxPayloadSize: udpMaxPayloadSize,
    }
    
    // 创建转发器
    for _, target := range targets {
        if !target.Enable {
            continue
        }
        
        forwarder := srt.New(target.URL, &target, parent, writeTimeout, udpMaxPayloadSize)
        m.forwarders = append(m.forwarders, forwarder)
    }
    
    return m
}

func (m *Manager) Start(stream *stream.Stream) {
    m.stream = stream
    
    for _, f := range m.forwarders {
        go func(forwarder Forwarder) {
            err := forwarder.Start(stream)
            if err != nil {
                m.logger.Log(logger.Warn, "failed to start SRT forwarder %s: %v", forwarder.GetTarget(), err)
            }
        }(f)
    }
}

func (m *Manager) Stop() {
    m.ctxCancel()
    
    for _, f := range m.forwarders {
        f.Stop()
    }
}

func (m *Manager) GetStats() []Stats {
    var stats []Stats
    for _, f := range m.forwarders {
        stats = append(stats, f.GetStats())
    }
    return stats
}
```

## 5. 集成到 Path

### 5.1 修改 path.go

```go
// 在 path struct 中添加
type path struct {
    // ... 现有字段 ...
    forwarderManager *forwarder.Manager
}

// 在 initialize() 中
func (pa *path) initialize() {
    // ... 现有代码 ...
    
    // 初始化转发器管理器
    if len(pa.conf.SRTForwardTargets) > 0 {
        pa.forwarderManager = forwarder.NewManager(
            pa.ctx,
            pa.conf.SRTForwardTargets,
            nil, // stream 稍后设置
            pa,
            time.Duration(pa.writeTimeout),
            pa.udpMaxPayloadSize,
        )
    }
}

// 在 onStreamReady() 中
func (pa *path) onStreamReady() {
    // ... 现有代码 ...
    
    // 启动转发器
    if pa.forwarderManager != nil && pa.stream != nil {
        pa.forwarderManager.Start(pa.stream)
    }
}

// 在 onStreamNotReady() 中
func (pa *path) onStreamNotReady() {
    // ... 现有代码 ...
    
    // 停止转发器
    if pa.forwarderManager != nil {
        pa.forwarderManager.Stop()
    }
}

// 在 close() 中
func (pa *path) close() {
    // ... 现有代码 ...
    
    if pa.forwarderManager != nil {
        pa.forwarderManager.Stop()
    }
}
```

## 6. 配置验证

### 6.1 在 conf/path.go 中添加验证

```go
func (pconf *Path) Check(confName string, defaults *Path) error {
    // ... 现有验证代码 ...
    
    // 验证 SRTForwardTargets
    for i, target := range pconf.SRTForwardTargets {
        if target.URL == "" {
            return fmt.Errorf("srtForwardTargets[%d]: url is required", i)
        }
        
        // 验证 URL 格式
        srtConf := srt.DefaultConfig()
        _, err := srtConf.UnmarshalURL(target.URL)
        if err != nil {
            return fmt.Errorf("srtForwardTargets[%d]: invalid SRT URL: %w", i, err)
        }
        
        if target.Reconnect && target.ReconnectDelay <= 0 {
            return fmt.Errorf("srtForwardTargets[%d]: reconnectDelay must be > 0 when reconnect is enabled", i)
        }
    }
    
    return nil
}
```

## 7. 实施步骤

### Phase 1: 基础框架
1. 创建 `internal/forwarder` 目录结构
2. 实现 Forwarder 接口和基础结构
3. 实现 Manager

### Phase 2: SRT Forwarder
1. 实现 SRT Forwarder
2. 集成到 Path 生命周期
3. 添加配置验证

### Phase 3: 测试和优化
1. 单元测试
2. 集成测试
3. 性能优化

## 8. 注意事项

### 8.1 延迟控制
- SRT 的 latency 参数影响延迟
- 默认 120ms，可根据需求调整

### 8.2 错误处理
- 网络断开时自动重连
- 记录错误日志和统计信息

### 8.3 资源管理
- 及时释放 SRT 连接
- 正确清理 Reader

### 8.4 性能考虑
- 使用 buffered writer 提高性能
- 避免阻塞主 Stream 处理

## 9. 未来扩展

### 9.1 统计和监控
- 转发成功率、延迟统计
- 集成到现有的 metrics 系统

### 9.2 动态配置
- 支持通过 API 动态添加/删除转发目标

### 9.3 条件转发
- 根据流质量、客户端数量等条件决定是否转发

