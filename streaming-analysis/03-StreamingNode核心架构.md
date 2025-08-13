# StreamingNode 核心架构分析

## 1. 概述

StreamingNode 是 Milvus 流处理系统的执行节点，负责：
- WAL（Write-Ahead Log）管理和存储
- 消息的生产和消费
- 数据刷写到持久化存储
- 事务和时间戳管理
- 故障恢复和检查点

## 2. 架构设计

### 2.1 服务器结构

```go
type Server struct {
    // 会话管理
    session    *sessionutil.Session
    grpcServer *grpc.Server
    
    // 服务层实例
    handlerService service.HandlerService  // 处理服务（生产/消费）
    managerService service.ManagerService  // 管理服务
    
    // 核心组件
    walManager walmanager.Manager  // WAL 管理器
}
```

### 2.2 初始化流程

```go
func (s *Server) init() {
    // 1. 初始化基础组件
    s.initBasicComponent()
    
    // 2. 初始化服务
    s.initService()
    
    // 3. 初始化存储系统
    initcore.InitStorageV2FileSystem(paramtable.Get())
}
```

## 3. WAL 系统架构

### 3.1 WAL 接口定义

```go
// 读写 WAL 接口
type WAL interface {
    ROWAL
    
    // 获取最新的 MVCC 时间戳
    GetLatestMVCCTimestamp(ctx context.Context, vchannel string) (uint64, error)
    
    // 同步追加消息
    Append(ctx context.Context, msg message.MutableMessage) (*AppendResult, error)
    
    // 异步追加消息
    AppendAsync(ctx context.Context, msg message.MutableMessage, 
                cb func(*AppendResult, error))
}

// 只读 WAL 接口
type ROWAL interface {
    WALName() string
    Metrics() types.WALMetrics
    Channel() types.PChannelInfo
    Read(ctx context.Context, deliverPolicy ReadOption) (Scanner, error)
    Available() <-chan struct{}
    IsAvailable() bool
    Close()
}
```

### 3.2 WAL 适配器架构

```go
type walAdaptorImpl struct {
    *roWALAdaptorImpl  // 只读部分
    
    rwWALImpls          walimpls.WALImpls           // 底层实现
    appendExecutionPool *conc.Pool[struct{}]        // 追加执行池
    param               *interceptors.InterceptorBuildParam
    interceptorBuildResult interceptorBuildResult   // 拦截器
    flusher             *flusherimpl.WALFlusherImpl // 刷写器
    writeMetrics        *metricsutil.WriteMetrics   // 写入指标
    isFenced            *atomic.Bool                // 是否被隔离
}
```

### 3.3 拦截器链机制

#### 3.3.1 拦截器接口

```go
type Interceptor interface {
    // 执行追加操作
    DoAppend(ctx context.Context, msg message.MutableMessage, 
             append Append) (message.MessageID, error)
    
    // 关闭拦截器
    Close()
}
```

#### 3.3.2 拦截器类型

1. **Chain Interceptor**：链式拦截器容器
2. **Lock Interceptor**：并发控制
3. **Shard Interceptor**：分片管理
4. **TimeTick Interceptor**：时间戳同步
5. **Redo Interceptor**：重做日志
6. **Txn Interceptor**：事务管理
7. **WAB (Write-Ahead Buffer) Interceptor**：写前缓冲

#### 3.3.3 拦截器链执行流程

```go
func chainAppendInterceptors(interceptors []Interceptor) AppendInterceptorCall {
    return func(ctx context.Context, msg message.MutableMessage, 
                invoker Append) (message.MessageID, error) {
        // 递归构建拦截器调用链
        return interceptors[0].DoAppend(ctx, msg, 
            getChainAppendInvoker(interceptors, 0, invoker))
    }
}
```

## 4. WAL Manager（WAL 管理器）

### 4.1 Manager 接口

```go
type Manager interface {
    // 打开 WAL 实例
    Open(ctx context.Context, channel types.PChannelInfo) error
    
    // 获取可用的 WAL
    GetAvailableWAL(channel types.PChannelInfo) (wal.WAL, error)
    
    // 获取指标
    Metrics() (*types.StreamingNodeMetrics, error)
    
    // 移除 WAL
    Remove(ctx context.Context, channel types.PChannelInfo) error
    
    // 关闭管理器
    Close()
}
```

### 4.2 WAL 生命周期管理

```go
type walLifetime struct {
    channel    types.PChannelInfo
    wal        wal.WAL
    state      walState
    closeNotifier *lifetime.Closer[struct{}]
}
```

**状态转换**：
```
Initializing -> Working -> Stopping -> Stopped
                   |
                   v
                Failed
```

### 4.3 WAL 状态管理

```go
type walState interface {
    CurrentState() walStateCode
    TransitTo(state walStateCode) error
}
```

## 5. Flusher 系统

### 5.1 WAL Flusher

负责将 WAL 中的数据刷写到持久化存储：

```go
type WALFlusher struct {
    vchannel      string
    segmentManager shards.SegmentManager
    dataCoordClient types.DataCoordClient
    msgHandler    MessageHandler
}
```

### 5.2 消息处理器

```go
type MessageHandler interface {
    HandleCreateCollection(ctx context.Context, msg *CreateCollectionMessage) error
    HandleDropCollection(ctx context.Context, msg *DropCollectionMessage) error
    HandleCreatePartition(ctx context.Context, msg *CreatePartitionMessage) error
    HandleDropPartition(ctx context.Context, msg *DropPartitionMessage) error
    HandleInsert(ctx context.Context, msg *InsertMessage) error
    HandleDelete(ctx context.Context, msg *DeleteMessage) error
}
```

### 5.3 刷写流程

1. 从 WAL 读取消息
2. 根据消息类型分发处理
3. 累积数据到 Segment
4. 触发刷写条件时执行刷写
5. 更新检查点

## 6. 服务层实现

### 6.1 Handler Service

处理数据读写请求：

```go
type HandlerService interface {
    // 生产消息
    Produce(server StreamingNodeHandlerService_ProduceServer) error
    
    // 消费消息
    Consume(req *ConsumeRequest, 
            server StreamingNodeHandlerService_ConsumeServer) error
}
```

#### 6.1.1 Producer 实现

```go
type Producer struct {
    wal        wal.WAL
    grpcServer ProduceServer
}

// 生产流程
func (p *Producer) Produce(msg Message) error {
    // 1. 验证消息
    // 2. 追加到 WAL
    // 3. 返回结果
}
```

#### 6.1.2 Consumer 实现

```go
type Consumer struct {
    scanner    wal.Scanner
    grpcServer ConsumeServer
}

// 消费流程
func (c *Consumer) Consume() error {
    // 1. 创建 Scanner
    // 2. 读取消息
    // 3. 推送给客户端
}
```

### 6.2 Manager Service

管理 WAL 生命周期：

```go
type ManagerService interface {
    // 分配 WAL
    Assign(ctx context.Context, req *AssignRequest) (*AssignResponse, error)
    
    // 移除 WAL
    Remove(ctx context.Context, req *RemoveRequest) (*RemoveResponse, error)
}
```

## 7. 恢复机制

### 7.1 检查点管理

```go
type Checkpoint struct {
    ChannelName  string
    RecoveryInfo VChannelRecoveryInfo
    SegmentInfo  map[int64]SegmentRecoveryInfo
}
```

### 7.2 恢复流程

```go
func RecoverWAL(ctx context.Context, channel PChannelInfo) (*WAL, error) {
    // 1. 加载检查点
    checkpoint := LoadCheckpoint(channel)
    
    // 2. 创建恢复流
    recoveryStream := CreateRecoveryStream(checkpoint)
    
    // 3. 重放 WAL
    ReplayWAL(recoveryStream)
    
    // 4. 恢复完成
    return CreateWAL(channel), nil
}
```

### 7.3 恢复存储接口

```go
type RecoveryStorage interface {
    // 获取恢复信息
    GetRecoveryInfo(vchannel string) (*VChannelRecoveryInfo, error)
    
    // 保存恢复信息
    PutRecoveryInfo(vchannel string, info *VChannelRecoveryInfo) error
    
    // 获取段恢复信息
    GetSegmentRecoveryInfo(segmentID int64) (*SegmentRecoveryInfo, error)
}
```

## 8. 事务支持

### 8.1 事务管理器

```go
type TxnManager struct {
    sessions sync.Map // txnID -> Session
    mvcc     MVCCManager
}
```

### 8.2 事务会话

```go
type Session struct {
    txnID      string
    state      TxnState
    beginTS    uint64
    commitTS   uint64
    operations []Operation
}
```

### 8.3 MVCC 管理

```go
type MVCCManager interface {
    // 获取 VChannel 的 MVCC 信息
    GetMVCCOfVChannel(vchannel string) MVCCInfo
    
    // 更新 MVCC 时间戳
    UpdateMVCCTimestamp(vchannel string, ts uint64)
}
```

## 9. 监控和指标

### 9.1 写入指标

```go
type WriteMetrics struct {
    // 追加延迟
    AppendLatency prometheus.Histogram
    
    // 消息大小
    MessageSize prometheus.Histogram
    
    // 吞吐量
    Throughput prometheus.Counter
}
```

### 9.2 扫描指标

```go
type ScanMetrics struct {
    // 扫描延迟
    ScanLatency prometheus.Histogram
    
    // 消息数量
    MessageCount prometheus.Counter
}
```

### 9.3 恢复指标

```go
type RecoveryMetrics struct {
    // 恢复时间
    RecoveryDuration prometheus.Histogram
    
    // 恢复的消息数
    RecoveredMessages prometheus.Counter
}
```

## 10. 高级特性

### 10.1 分片管理

**Shard Manager**：
- 管理 Collection、Partition、Segment 的层次结构
- 处理段的创建、封存、刷写
- 实现段的限制策略

### 10.2 时间戳同步

**TimeTick Inspector**：
- 定期检查时间戳进度
- 触发时间戳同步
- 处理时间戳超时

### 10.3 写前缓冲（WAB）

**Write-Ahead Buffer**：
- 缓冲待写入的消息
- 支持批量写入优化
- 处理背压控制

## 11. 性能优化

### 11.1 并发优化

- 使用对象池减少内存分配
- 批量处理消息
- 异步 I/O 操作

### 11.2 内存管理

- 消息堆用于排序
- 重排序缓冲区
- 智能内存释放

### 11.3 网络优化

- gRPC 流式传输
- 消息压缩
- 连接复用

## 12. 容错设计

### 12.1 故障检测

- 心跳机制
- 健康检查
- 自动故障转移

### 12.2 数据保护

- WAL 持久化
- 多副本支持
- 检查点备份

### 12.3 优雅关闭

- 等待正在进行的操作
- 保存当前状态
- 清理资源