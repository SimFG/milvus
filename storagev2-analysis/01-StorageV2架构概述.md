# Milvus StorageV2 架构概述

## 1. 概述

StorageV2 是 Milvus 新一代存储系统的核心组件，旨在提供高性能、可扩展的数据存储和检索能力。它基于 Apache Arrow 格式，提供了统一的数据格式和高效的内存管理机制。

## 2. 整体架构

### 2.1 架构分层

```
┌─────────────────────────────────────────────────┐
│                   应用层                         │
│  (QueryNode, DataNode, IndexNode)              │
├─────────────────────────────────────────────────┤
│                  StorageV2 层                   │
│  ┌─────────────┐  ┌─────────────┐              │
│  │ PackedReader│  │PackedWriter │              │
│  └─────────────┘  └─────────────┘              │
│  ┌─────────────────────────────────────────────┐ │
│  │           Arrow Format & C API             │ │
│  └─────────────────────────────────────────────┘ │
├─────────────────────────────────────────────────┤
│                  存储抽象层                      │
│  ┌─────────────┐  ┌─────────────┐              │
│  │ChunkManager │  │  FileManager│              │
│  └─────────────┘  └─────────────┘              │
├─────────────────────────────────────────────────┤
│                 物理存储层                       │
│  ┌─────────────┐  ┌─────────────┐  ┌─────────┐ │
│  │   Local     │  │    MinIO    │  │ Cloud   │ │
│  │  Storage    │  │   Storage   │  │Storage  │ │
│  └─────────────┘  └─────────────┘  └─────────┘ │
└─────────────────────────────────────────────────┘
```

### 2.2 核心组件

#### 2.2.1 PackedReader/Writer
```go
// 核心读取器
type PackedReader struct {
    cPackedReader C.CPackedReader
    arr           *cdata.CArrowArray
    schema        *arrow.Schema
    currentBatch  arrow.Record
}

// 核心写入器
type PackedWriter struct {
    cPackedWriter C.CPackedWriter
}
```

#### 2.2.2 存储配置
```go
type CStorageConfig struct {
    address                C.CString  // 存储地址
    bucket_name            C.CString  // 桶名称
    access_key_id          C.CString  // 访问密钥ID
    access_key_value       C.CString  // 访问密钥值
    root_path              C.CString  // 根路径
    storage_type           C.CString  // 存储类型
    cloud_provider         C.CString  // 云提供商
    useSSL                 C.bool     // 是否使用SSL
    useIAM                 C.bool     // 是否使用IAM
    region                 C.CString  // 区域
    requestTimeoutMs       C.int64_t  // 请求超时
    use_custom_part_upload C.bool     // 自定义分片上传
}
```

## 3. 设计理念

### 3.1 基于 Arrow 的统一格式

StorageV2 采用 Apache Arrow 作为内存中的数据表示格式：

```go
// Arrow Schema 转换
func ConvertToArrowSchema(schema *schemapb.CollectionSchema) (*arrow.Schema, error)

// Arrow Record 处理
func (pw *packedRecordWriter) Write(r Record) error {
    var rec arrow.Record
    if sar, ok := r.(*simpleArrowRecord); ok {
        rec = sar.r
    } else {
        // 构建 Arrow Record
        arrays := make([]Arrow.Array, len(pw.schema.Fields))
        for i, field := range pw.schema.Fields {
            arrays[i] = r.Column(field.FieldID)
        }
        rec = array.NewRecord(pw.arrowSchema, arrays, int64(r.Len()))
    }
    
    return pw.writer.WriteRecordBatch(rec)
}
```

### 3.2 列式存储优化

#### 3.2.1 列分组存储
```go
type ColumnGroup struct {
    GroupID typeutil.UniqueID
    Columns []int  // 列索引列表
}

// 按 Schema 分组
func SplitBySchema(fields []*schemapb.FieldSchema) []ColumnGroup
```

#### 3.2.2 压缩和编码
- 支持多种压缩算法（ZSTD、LZ4等）
- 针对不同数据类型优化编码方式
- 自适应压缩策略

### 3.3 C++ 和 Go 混合架构

#### 3.3.1 C API 接口
```c
// C API 定义
typedef struct CPackedReader CPackedReader;
typedef struct CPackedWriter CPackedWriter;

// 创建读取器
CStatus NewPackedReader(
    const char** file_paths,
    int64_t num_paths,
    struct ArrowSchema* schema,
    int64_t buffer_size,
    CPackedReader* reader
);

// 读取下一批数据
CStatus ReadNext(
    CPackedReader reader,
    struct ArrowArray* array,
    struct ArrowSchema* schema
);
```

#### 3.3.2 Go 封装层
```go
// CGO 绑定
/*
#cgo pkg-config: milvus_core
#include "segcore/packed_reader_c.h"
#include "arrow/c/abi.h"
*/
import "C"

func NewPackedReader(
    filePaths []string,
    schema *arrow.Schema,
    bufferSize int64,
    storageConfig *indexpb.StorageConfig,
) (*PackedReader, error) {
    // C 字符串转换
    cFilePaths := make([]*C.char, len(filePaths))
    for i, path := range filePaths {
        cFilePaths[i] = C.CString(path)
        defer C.free(unsafe.Pointer(cFilePaths[i]))
    }
    
    // Arrow Schema 导出
    var cas cdata.CArrowSchema
    cdata.ExportArrowSchema(schema, &cas)
    
    // 调用 C API
    status := C.NewPackedReader(/* ... */)
    return &PackedReader{/* ... */}, nil
}
```

## 4. 核心特性

### 4.1 高性能数据访问

#### 4.1.1 零拷贝数据传输
- 基于 Arrow C Data Interface
- 内存映射文件访问
- 直接内存操作避免序列化开销

#### 4.1.2 批处理优化
```go
func (pr *PackedReader) ReadNext() (arrow.Record, error) {
    // 批量读取优化
    var cArr C.CArrowArray
    var cSchema C.CArrowSchema
    status := C.ReadNext(pr.cPackedReader, &cArr, &cSchema)
    
    if cArr == nil {
        return nil, io.EOF
    }
    
    // 零拷贝转换
    goCArr := (*cdata.CArrowArray)(unsafe.Pointer(cArr))
    goCSchema := (*cdata.CArrowSchema)(unsafe.Pointer(cSchema))
    
    recordBatch, err := cdata.ImportCRecordBatch(goCArr, goCSchema)
    return recordBatch, nil
}
```

### 4.2 多存储后端支持

#### 4.2.1 存储抽象
```cpp
class ChunkManager {
public:
    virtual bool Exist(const std::string& filepath) = 0;
    virtual uint64_t Size(const std::string& filepath) = 0;
    virtual uint64_t Read(const std::string& filepath, void* buf, uint64_t len) = 0;
    virtual void Write(const std::string& filepath, void* buf, uint64_t len) = 0;
    virtual std::vector<std::string> ListWithPrefix(const std::string& filepath) = 0;
    virtual void Remove(const std::string& filepath) = 0;
};
```

#### 4.2.2 具体实现
- **本地存储**: LocalChunkManager
- **MinIO**: MinioChunkManager  
- **云存储**: 
  - Azure Blob: AzureBlobChunkManager
  - GCP Storage: GcpNativeChunkManager
  - AWS S3: 通过 MinIO 兼容接口

### 4.3 灵活的数据组织

#### 4.3.1 文件分片
```go
// 多文件写入支持
func NewPackedWriter(
    filePaths []string,           // 多个文件路径
    schema *arrow.Schema,
    bufferSize int64,
    multiPartUploadSize int64,    // 分片上传大小
    columnGroups []ColumnGroup,   // 列分组
    storageConfig *StorageConfig,
) (*PackedWriter, error)
```

#### 4.3.2 列分组策略
```go
// 自动列分组
func SplitBySchema(fields []*schemapb.FieldSchema) []ColumnGroup {
    var groups []ColumnGroup
    for i, field := range fields {
        group := ColumnGroup{
            GroupID: field.FieldID,
            Columns: []int{i},
        }
        groups = append(groups, group)
    }
    return groups
}
```

## 5. 性能优化

### 5.1 内存管理

#### 5.1.1 缓冲区管理
- 可配置的缓冲区大小
- 内存复用机制
- 垃圾回收优化

#### 5.1.2 异步 I/O
- 多线程读写
- 异步刷新机制
- 流水线处理

### 5.2 网络优化

#### 5.2.1 分片上传
```go
// 大文件分片上传
cMultiPartUploadSize := C.int64_t(multiPartUploadSize)
status = C.NewPackedWriterWithStorageConfig(
    cSchema,
    cBufferSize,
    cFilePathsArray,
    cNumPaths,
    cMultiPartUploadSize,  // 分片大小
    cColumnGroups,
    cStorageConfig,
    &cPackedWriter
)
```

#### 5.2.2 连接复用
- HTTP/2 支持
- 连接池管理
- 自动重连机制

## 6. 与传统存储的对比

| 特性 | StorageV1 | StorageV2 |
|------|-----------|-----------|
| 数据格式 | 自定义二进制 | Apache Arrow |
| 内存管理 | 手动管理 | Arrow 内存池 |
| 跨语言支持 | 有限 | 完全支持 |
| 性能 | 一般 | 高性能 |
| 可扩展性 | 受限 | 高可扩展 |
| 生态兼容性 | 无 | 丰富的 Arrow 生态 |

## 7. 应用场景

### 7.1 数据摄入
- 高吞吐量数据写入
- 批量数据导入
- 实时数据流处理

### 7.2 查询处理
- 列式数据扫描
- 向量检索优化
- 聚合计算加速

### 7.3 索引构建
- 向量索引数据准备
- 标量索引构建
- 多模态索引支持

## 8. 未来发展方向

### 8.1 性能优化
- GPU 加速支持
- SIMD 指令优化
- 更高效的压缩算法

### 8.2 功能扩展
- 更多存储后端
- 事务支持
- 数据版本管理

### 8.3 生态集成
- 与 Arrow Flight 集成
- Parquet 格式支持
- 数据湖集成

StorageV2 代表了 Milvus 存储系统的重大升级，通过采用现代化的设计理念和技术栈，为向量数据库提供了强大的存储基础设施。