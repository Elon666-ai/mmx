# SRT 无延迟转发功能设计文档索引

## 文档结构

1. **[srt-forward-design.md](./srt-forward-design.md)** - SRT 转发设计文档
   - 概述和目标
   - 架构设计
   - 配置设计
   - 实现细节
   - 实施步骤

## 设计要点总结

### 核心思想
- **零延迟转发**：直接复用 MPEG-TS 封装逻辑，不重新编码
- **SRT 到 SRT**：只实现 SRT 协议转发
- **高可用性**：自动重连、错误恢复

### 技术方案
1. **复用 Stream.Reader 机制**：转发器作为 Stream 的 Reader，通过回调接收数据
2. **复用 MPEG-TS 封装**：直接使用 `mpegts.FromStream()` 函数
3. **SRT 客户端连接**：使用 `github.com/datarhei/gosrt` 库建立连接

### 关键组件
- `internal/forwarder/manager.go` - 转发器管理器
- `internal/forwarder/forwarder.go` - 转发器接口
- `internal/forwarder/srt/forwarder.go` - SRT 转发器实现

### 配置示例

```yaml
paths:
  mystream:
    srtForwardTargets:
      - url: srt://remote-server:8890?streamid=publish:target-path
        enable: yes
        reconnect: yes
        reconnectDelay: 2s
        passphrase: mypassphrase
        latency: 120
        packetSize: 1316
```

### 实施步骤

1. **Phase 1**: 基础框架和接口定义
2. **Phase 2**: SRT 转发器实现
3. **Phase 3**: 集成到 Path 生命周期
4. **Phase 4**: 测试和优化

>>
增加转码功能，要求：

