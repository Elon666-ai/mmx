# 转发器实现细节

## 1. 目录结构

```
internal/
├── forwarder/
│   ├── manager.go          # 转发器管理器
│   ├── forwarder.go        # 转发器接口定义
│   ├── base.go             # 基础转发器实现
│   ├── stats.go            # 统计信息
│   ├── webrtc/
│   │   └── forwarder.go    # WebRTC 转发器
│   ├── rtsp/
│   │   └── forwarder.go    # RTSP 转发器
│   ├── srt/
│   │   └── forwarder.go    # SRT 转发器
│   └── rtmp/
│       └── forwarder.go    # RTMP 转发器
```

## 2. 核心接口定义

### 2.1 Forwarder 接口

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

### 2.2 Base Forwarder

```go
package forwarder

import (
    "context"
    "sync"
    "time"
    "github.com/bluenviron/mediamtx/internal/stream"
    "github.com/bluenviron/mediamtx/internal/logger"
    "github.com/bluenviron/mediamtx/internal/conf"
)

// Base 是转发器的基础实现
type Base struct {
    url           string
    config        *conf.ForwardTarget
    stream        *stream.Stream
    reader        *stream.Reader
    logger        logger.Writer
    ctx           context.Context
    ctxCancel     context.CancelFunc
    wg            sync.WaitGroup
    mutex         sync.RWMutex
    
    // 统计信息
    bytesSent     uint64
    packetsSent   uint64
    packetsLost   uint64
    lastError     error
    connected     bool
    reconnectCount uint64
}

func NewBase(url string, config *conf.ForwardTarget, parent logger.Writer) *Base {
    ctx, ctxCancel := context.WithCancel(context.Background())
    
    return &Base{
        url:       url,
        config:    config,
        logger:    parent,
        ctx:       ctx,
        ctxCancel: ctxCancel,
    }
}

func (b *Base) Start(stream *stream.Stream) error {
    b.mutex.Lock()
    defer b.mutex.Unlock()
    
    if b.stream != nil {
        return fmt.Errorf("forwarder already started")
    }
    
    b.stream = stream
    b.reader = &stream.Reader{Parent: b}
    
    b.wg.Add(1)
    go b.run()
    
    return nil
}

func (b *Base) Stop() {
    b.ctxCancel()
    b.wg.Wait()
    
    if b.reader != nil && b.stream != nil {
        b.stream.RemoveReader(b.reader)
    }
}

func (b *Base) IsRunning() bool {
    b.mutex.RLock()
    defer b.mutex.RUnlock()
    return b.stream != nil
}

func (b *Base) GetStats() Stats {
    b.mutex.RLock()
    defer b.mutex.RUnlock()
    
    return Stats{
        BytesSent:     atomic.LoadUint64(&b.bytesSent),
        PacketsSent:   atomic.LoadUint64(&b.packetsSent),
        PacketsLost:   atomic.LoadUint64(&b.packetsLost),
        LastError:     b.lastError,
        Connected:     b.connected,
        ReconnectCount: atomic.LoadUint64(&b.reconnectCount),
    }
}

func (b *Base) GetTarget() string {
    return b.url
}

// run 是转发器的主循环，由子类实现
func (b *Base) run() {
    defer b.wg.Done()
    
    for {
        select {
        case <-b.ctx.Done():
            return
        default:
            err := b.runInner()
            if err != nil {
                b.mutex.Lock()
                b.lastError = err
                b.connected = false
                b.mutex.Unlock()
                
                b.logger.Log(logger.Warn, "forwarder error: %v", err)
                
                if b.config.Reconnect {
                    atomic.AddUint64(&b.reconnectCount, 1)
                    time.Sleep(time.Duration(b.config.ReconnectDelay))
                    continue
                }
                return
            }
        }
    }
}

// runInner 由子类实现具体的转发逻辑
func (b *Base) runInner() error {
    return fmt.Errorf("not implemented")
}
```

## 3. WebRTC Forwarder 实现

### 3.1 完整实现

```go
package webrtc

import (
    "context"
    "fmt"
    "net/http"
    "time"
    
    "github.com/bluenviron/gortsplib/v5/pkg/description"
    "github.com/bluenviron/gortsplib/v5/pkg/format"
    "github.com/bluenviron/mediamtx/internal/conf"
    "github.com/bluenviron/mediamtx/internal/forwarder"
    "github.com/bluenviron/mediamtx/internal/logger"
    "github.com/bluenviron/mediamtx/internal/protocols/webrtc"
    "github.com/bluenviron/mediamtx/internal/stream"
    "github.com/pion/webrtc/v4"
)

type Forwarder struct {
    *forwarder.Base
    pc     *webrtc.PeerConnection
    tracks []*webrtc.OutgoingTrack
}

func New(url string, config *conf.ForwardTarget, parent logger.Writer) *Forwarder {
    return &Forwarder{
        Base: forwarder.NewBase(url, config, parent),
    }
}

func (f *Forwarder) runInner() error {
    // 解析 URL
    u, err := parseWHEPURL(f.GetTarget())
    if err != nil {
        return err
    }
    
    // 创建 PeerConnection
    pc, err := f.createPeerConnection()
    if err != nil {
        return err
    }
    f.pc = pc
    
    // 设置 tracks
    err = f.setupTracks()
    if err != nil {
        return err
    }
    
    // 创建 Offer
    offer, err := pc.CreateOffer(nil)
    if err != nil {
        return err
    }
    
    err = pc.SetLocalDescription(offer)
    if err != nil {
        return err
    }
    
    // 发送 Offer 到远程服务器（WHEP）
    answer, err := f.sendWHEPOffer(u, offer)
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
    
    // 标记为已连接
    f.mutex.Lock()
    f.connected = true
    f.mutex.Unlock()
    
    // 添加 Reader 到 Stream
    f.stream.AddReader(f.reader)
    
    // 等待连接失败或上下文取消
    select {
    case <-pc.Failed():
        return fmt.Errorf("peer connection failed")
    case <-f.ctx.Done():
        return nil
    }
}

func (f *Forwarder) createPeerConnection() (*webrtc.PeerConnection, error) {
    config := webrtc.Configuration{
        ICEServers: []webrtc.ICEServer{},
    }
    
    // 添加 ICE servers
    if f.config.WebRTCConfig != nil {
        for _, server := range f.config.WebRTCConfig.ICEServers {
            config.ICEServers = append(config.ICEServers, webrtc.ICEServer{
                URLs: []string{server},
            })
        }
    }
    
    pc, err := webrtc.NewPeerConnection(config)
    if err != nil {
        return nil, err
    }
    
    return pc, nil
}

func (f *Forwarder) setupTracks() error {
    if f.stream == nil {
        return fmt.Errorf("stream not set")
    }
    
    for _, media := range f.stream.Desc.Medias {
        for _, format := range media.Formats {
            // 创建 OutgoingTrack
            track, err := f.createOutgoingTrack(media, format)
            if err != nil {
                return err
            }
            f.tracks = append(f.tracks, track)
            
            // 注册数据回调
            cMedia := media
            cFormat := format
            f.reader.OnData(cMedia, cFormat, func(u *unit.Unit) error {
                if u.NilPayload() {
                    return nil
                }
                
                for _, pkt := range u.RTPPackets {
                    err := track.WriteRTPWithNTP(pkt, u.NTP)
                    if err != nil {
                        atomic.AddUint64(&f.packetsLost, 1)
                        return err
                    }
                    atomic.AddUint64(&f.packetsSent, 1)
                    atomic.AddUint64(&f.bytesSent, uint64(len(pkt.Marshal())))
                }
                return nil
            })
        }
    }
    
    return nil
}

func (f *Forwarder) createOutgoingTrack(media *description.Media, format format.Format) (*webrtc.OutgoingTrack, error) {
    // 获取 codec 信息
    codec := format.Codec()
    
    // 创建 TrackLocalStaticRTP
    trackID := "video"
    if media.Type == description.MediaTypeAudio {
        trackID = "audio"
    }
    
    track, err := webrtc.NewTrackLocalStaticRTP(
        webrtc.RTPCodecCapability{
            MimeType:  codec.MimeType(),
            ClockRate: uint32(codec.ClockRate()),
        },
        trackID,
        "forwarder",
    )
    if err != nil {
        return nil, err
    }
    
    // 添加到 PeerConnection
    sender, err := f.pc.AddTrack(track)
    if err != nil {
        return nil, err
    }
    
    // 读取 RTCP（必须）
    go func() {
        buf := make([]byte, 1500)
        for {
            n, _, err := sender.Read(buf)
            if err != nil {
                return
            }
            // 处理 RTCP 包
            _ = n
        }
    }()
    
    return track, nil
}

func (f *Forwarder) sendWHEPOffer(url string, offer *webrtc.SessionDescription) (*webrtc.SessionDescription, error) {
    // 发送 HTTP POST 请求到 WHEP 端点
    req, err := http.NewRequest("POST", url, strings.NewReader(offer.SDP))
    if err != nil {
        return nil, err
    }
    
    req.Header.Set("Content-Type", "application/sdp")
    
    client := &http.Client{Timeout: 10 * time.Second}
    resp, err := client.Do(req)
    if err != nil {
        return nil, err
    }
    defer resp.Body.Close()
    
    if resp.StatusCode != http.StatusCreated {
        return nil, fmt.Errorf("WHEP offer failed: %d", resp.StatusCode)
    }
    
    // 读取 Answer
    body, err := io.ReadAll(resp.Body)
    if err != nil {
        return nil, err
    }
    
    answer := &webrtc.SessionDescription{
        Type: webrtc.SDPTypeAnswer,
        SDP:  string(body),
    }
    
    return answer, nil
}

func (f *Forwarder) Stop() {
    if f.pc != nil {
        f.pc.Close()
    }
    f.Base.Stop()
}
```

## 4. RTSP Forwarder 实现

### 4.1 完整实现

```go
package rtsp

import (
    "fmt"
    "time"
    
    "github.com/bluenviron/gortsplib/v5"
    "github.com/bluenviron/gortsplib/v5/pkg/base"
    "github.com/bluenviron/gortsplib/v5/pkg/description"
    "github.com/bluenviron/gortsplib/v5/pkg/format"
    "github.com/bluenviron/mediamtx/internal/conf"
    "github.com/bluenviron/mediamtx/internal/forwarder"
    "github.com/bluenviron/mediamtx/internal/logger"
    "github.com/bluenviron/mediamtx/internal/stream"
    "github.com/bluenviron/mediamtx/internal/unit"
)

type Forwarder struct {
    *forwarder.Base
    client *gortsplib.Client
}

func New(url string, config *conf.ForwardTarget, parent logger.Writer) *Forwarder {
    return &Forwarder{
        Base: forwarder.NewBase(url, config, parent),
    }
}

func (f *Forwarder) runInner() error {
    // 解析 URL
    u, err := base.ParseURL(f.GetTarget())
    if err != nil {
        return err
    }
    
    // 创建 RTSP Client
    f.client = &gortsplib.Client{
        Protocol: f.config.RTSPConfig.Transport.Protocol,
        TLSConfig: nil, // 如果需要 TLS，在这里配置
    }
    
    // 设置认证
    if f.config.Username != "" {
        f.client.Auth = &gortsplib.Auth{
            Method: gortsplib.AuthBasic,
            User:   f.config.Username,
            Pass:   f.config.Password,
        }
    }
    
    err = f.client.Start()
    if err != nil {
        return err
    }
    defer f.client.Close()
    
    // 开始推流
    err = f.client.StartRecording(u, f.stream.Desc)
    if err != nil {
        return err
    }
    
    // 标记为已连接
    f.mutex.Lock()
    f.connected = true
    f.mutex.Unlock()
    
    // 设置数据回调
    for _, media := range f.stream.Desc.Medias {
        for _, format := range media.Formats {
            cMedia := media
            cFormat := format
            f.reader.OnData(cMedia, cFormat, func(u *unit.Unit) error {
                if u.NilPayload() {
                    return nil
                }
                
                for _, pkt := range u.RTPPackets {
                    err := f.client.WritePacketRTPWithNTP(cMedia, pkt, u.NTP)
                    if err != nil {
                        atomic.AddUint64(&f.packetsLost, 1)
                        return err
                    }
                    atomic.AddUint64(&f.packetsSent, 1)
                    atomic.AddUint64(&f.bytesSent, uint64(len(pkt.Marshal())))
                }
                return nil
            })
        }
    }
    
    // 添加 Reader 到 Stream
    f.stream.AddReader(f.reader)
    
    // 等待连接失败或上下文取消
    select {
    case <-f.client.Done():
        return fmt.Errorf("RTSP client closed")
    case <-f.ctx.Done():
        return nil
    }
}

func (f *Forwarder) Stop() {
    if f.client != nil {
        f.client.Close()
    }
    f.Base.Stop()
}
```

## 5. Manager 实现

### 5.1 完整实现

```go
package forwarder

import (
    "context"
    "fmt"
    "net/url"
    "strings"
    
    "github.com/bluenviron/mediamtx/internal/conf"
    "github.com/bluenviron/mediamtx/internal/forwarder/rtmp"
    "github.com/bluenviron/mediamtx/internal/forwarder/rtsp"
    "github.com/bluenviron/mediamtx/internal/forwarder/srt"
    "github.com/bluenviron/mediamtx/internal/forwarder/webrtc"
    "github.com/bluenviron/mediamtx/internal/logger"
    "github.com/bluenviron/mediamtx/internal/stream"
)

type Manager struct {
    forwarders []Forwarder
    stream     *stream.Stream
    logger     logger.Writer
    ctx        context.Context
    ctxCancel  context.CancelFunc
}

func NewManager(
    ctx context.Context,
    targets []conf.ForwardTarget,
    stream *stream.Stream,
    parent logger.Writer,
) *Manager {
    ctx, ctxCancel := context.WithCancel(ctx)
    
    m := &Manager{
        stream:    stream,
        logger:    parent,
        ctx:       ctx,
        ctxCancel: ctxCancel,
    }
    
    // 创建转发器
    for _, target := range targets {
        if !target.Enable {
            continue
        }
        
        forwarder := m.createForwarder(target, parent)
        if forwarder != nil {
            m.forwarders = append(m.forwarders, forwarder)
        }
    }
    
    return m
}

func (m *Manager) createForwarder(target conf.ForwardTarget, parent logger.Writer) Forwarder {
    u, err := url.Parse(target.URL)
    if err != nil {
        m.logger.Log(logger.Warn, "invalid forward URL: %s, error: %v", target.URL, err)
        return nil
    }
    
    scheme := strings.ToLower(u.Scheme)
    
    switch scheme {
    case "webrtc", "whep":
        return webrtc.New(target.URL, &target, parent)
    case "rtsp", "rtsps":
        return rtsp.New(target.URL, &target, parent)
    case "srt":
        return srt.New(target.URL, &target, parent)
    case "rtmp", "rtmps":
        return rtmp.New(target.URL, &target, parent)
    default:
        m.logger.Log(logger.Warn, "unsupported forward protocol: %s", scheme)
        return nil
    }
}

func (m *Manager) Start(stream *stream.Stream) {
    m.stream = stream
    
    for _, f := range m.forwarders {
        go func(forwarder Forwarder) {
            err := forwarder.Start(stream)
            if err != nil {
                m.logger.Log(logger.Warn, "failed to start forwarder %s: %v", forwarder.GetTarget(), err)
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

## 6. 集成到 Path

### 6.1 修改 path.go

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
    if len(pa.conf.ForwardTargets) > 0 {
        pa.forwarderManager = forwarder.NewManager(
            pa.ctx,
            pa.conf.ForwardTargets,
            nil, // stream 稍后设置
            pa,
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

## 7. 配置验证

### 7.1 在 conf/path.go 中添加验证

```go
func (pconf *Path) Check(confName string, defaults *Path) error {
    // ... 现有验证代码 ...
    
    // 验证 ForwardTargets
    for i, target := range pconf.ForwardTargets {
        if target.URL == "" {
            return fmt.Errorf("forwardTargets[%d]: url is required", i)
        }
        
        u, err := url.Parse(target.URL)
        if err != nil {
            return fmt.Errorf("forwardTargets[%d]: invalid URL: %w", i, err)
        }
        
        scheme := strings.ToLower(u.Scheme)
        switch scheme {
        case "webrtc", "whep", "rtsp", "rtsps", "srt", "rtmp", "rtmps":
            // 支持的协议
        default:
            return fmt.Errorf("forwardTargets[%d]: unsupported protocol: %s", i, scheme)
        }
        
        if target.Reconnect && target.ReconnectDelay <= 0 {
            return fmt.Errorf("forwardTargets[%d]: reconnectDelay must be > 0 when reconnect is enabled", i)
        }
    }
    
    return nil
}
```

## 8. API 扩展

### 8.1 添加转发统计 API

在 `internal/api/api_paths.go` 中添加：

```go
type APIPathForwarder struct {
    Target        string `json:"target"`
    Connected     bool   `json:"connected"`
    BytesSent     uint64 `json:"bytesSent"`
    PacketsSent   uint64 `json:"packetsSent"`
    PacketsLost   uint64 `json:"packetsLost"`
    ReconnectCount uint64 `json:"reconnectCount"`
    LastError     string `json:"lastError,omitempty"`
}

// 在 APIPath 中添加
type APIPath struct {
    // ... 现有字段 ...
    Forwarders []APIPathForwarder `json:"forwarders,omitempty"`
}
```

