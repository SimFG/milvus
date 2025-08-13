# Chunk大小限制机制详解

## 核心概念澄清

**重要说明**：Chunk的大小限制主要是通过**行数**来控制的，而不是内存大小。内存限制是用于控制**并行加载**和**批处理**的，不是用来决定单个Chunk的大小。

## 1. Chunk大小的真正决定因素

### 1.1 行数是主要限制

```cpp
// 默认配置
queryNode:
  segcore:
    chunkRows: 128  // 每个Chunk固定128行
```

**关键点**：
- 每个Chunk的行数是**固定的**（除了最后一个Chunk）
- 不管数据占用多少内存，都是按行数分片
- 即使128行数据超过4MB或128MB，仍然是一个Chunk

### 1.2 实际分片逻辑

```cpp
// 从Arrow RecordBatch创建Chunk的过程
void ChunkWriter::write(std::shared_ptr<arrow::RecordBatchReader> data) {
    // 1. 收集所有批次的数据
    auto batch_vec = data->ToRecordBatches().ValueOrDie();
    
    // 2. 累计所有行
    for (auto& batch : batch_vec) {
        row_nums_ += batch->num_rows();  // 累加行数
        auto data = batch->column(0);
        // 处理数据...
    }
    
    // 3. 创建一个包含所有行的Chunk
    // 注意：这里没有按内存大小分片！
    return std::make_unique<FixedWidthChunk>(
        row_nums_,    // 总行数
        dim_,
        data,
        size,          // 实际数据大小
        sizeof(T),
        nullable_
    );
}
```

## 2. 内存限制的真正用途

### 2.1 FILE_SLICE_SIZE (16MB)

```cpp
const int64_t DEFAULT_INDEX_FILE_SLICE_SIZE = 16 << 20;  // 16MB
int64_t FILE_SLICE_SIZE = DEFAULT_INDEX_FILE_SLICE_SIZE;
```

**用途**：控制从远程存储下载文件时的分片大小
- 不是用来限制Chunk大小
- 用于并行下载大文件
- 影响网络IO的批处理

```cpp
// 用于文件下载分片
void DiskFileManagerImpl::LoadBatchToMemory() {
    while (offset < fileSize) {
        auto batch_size = std::min(FILE_SLICE_SIZE, fileSize - offset);
        // 下载这个批次
        remote_file_sizes.emplace_back(batch_size);
        offset += batch_size;
    }
}
```

### 2.2 DEFAULT_FIELD_MAX_MEMORY_LIMIT (128MB)

```cpp
const int64_t DEFAULT_FIELD_MAX_MEMORY_LIMIT = 128 << 20;  // 128MB
```

**用途**：控制字段数据加载的并行度
- 不是单个Chunk的大小限制
- 决定同时加载多少个文件

```cpp
// 计算并行度
auto parallel_degree = DEFAULT_FIELD_MAX_MEMORY_LIMIT / FILE_SLICE_SIZE;
// = 128MB / 16MB = 8个并行任务

// 批量加载文件
for (auto& file : remote_files) {
    if (batch_files.size() >= parallel_degree) {
        FetchRawData();  // 并行加载这批文件
        batch_files.clear();
    }
    batch_files.emplace_back(file);
}
```

## 3. 实际的Chunk生成过程

### 3.1 Sealed Segment加载流程

```
1. 从binlog文件列表开始
   ├── file1.binlog (1000行)
   ├── file2.binlog (500行)
   └── file3.binlog (300行)
   总计：1800行

2. 按chunkRows=128分片
   ├── Chunk0: 128行
   ├── Chunk1: 128行
   ├── ...
   ├── Chunk13: 128行
   └── Chunk14: 36行 (1800 % 128)
```

### 3.2 实际代码流程

```cpp
// ChunkedSegmentSealedImpl::LoadFieldData
void LoadFieldData(const LoadFieldDataInfo& load_info) {
    for (auto& [id, info] : load_info.field_infos) {
        // 1. 并行加载Arrow数据（受内存限制控制）
        auto parallel_degree = DEFAULT_FIELD_MAX_MEMORY_LIMIT / FILE_SLICE_SIZE;
        
        // 2. 创建Column
        auto column = std::make_shared<ChunkedColumn>(field_meta);
        
        // 3. 按chunkRows创建Chunk
        int64_t total_rows = GetTotalRows();
        int64_t rows_processed = 0;
        
        while (rows_processed < total_rows) {
            int64_t chunk_rows = std::min(chunkRows, total_rows - rows_processed);
            
            // 创建固定行数的Chunk
            auto chunk = CreateChunkWithRows(chunk_rows, data + offset);
            column->AddChunk(chunk);
            
            rows_processed += chunk_rows;
        }
    }
}
```

## 4. 特殊情况说明

### 4.1 超大行的处理

**问题**：如果单行数据就超过了内存限制怎么办？

**答案**：仍然会创建Chunk，但可能触发以下情况：
1. 对于固定大小数据（如向量），按行数分片不受影响
2. 对于变长数据（如大字符串、大JSON），单个Chunk可能很大

```cpp
// 示例：超大JSON字段
// 假设每个JSON平均10MB，chunkRows=128
// 则一个Chunk = 128 * 10MB = 1.28GB！
// 这个Chunk仍然会被创建，但可能导致内存问题
```

### 4.2 Growing Segment的特殊处理

Growing Segment通常使用单个Chunk，动态增长：

```cpp
class SingleChunkColumn {
    void AppendBatch(const FieldDataPtr& data) {
        // 动态扩展，不受chunkRows限制
        // 直到Segment seal时才分片
    }
};
```

## 5. 配置建议

### 5.1 chunkRows的选择

```yaml
# 小数据场景（行数据小）
queryNode.segcore.chunkRows: 1024  # 可以设置更大

# 大数据场景（行数据大）
queryNode.segcore.chunkRows: 64    # 设置更小避免单个Chunk过大

# 默认值（平衡选择）
queryNode.segcore.chunkRows: 128   # 适合大多数场景
```

### 5.2 内存相关配置的影响

这些配置**不影响**Chunk大小，但影响加载性能：

```yaml
# 影响并行加载
common.storage.readBufferSizeInMB: 16    # FILE_SLICE_SIZE
dataNode.memory.maxFieldMemory: 128      # 字段内存限制

# 不会改变Chunk的行数！
```

## 6. 总结

### 关键要点

1. **Chunk大小由行数决定**，不是内存大小
   - 默认128行/Chunk
   - 最后一个Chunk可能不满

2. **内存限制用于控制并行和批处理**
   - FILE_SLICE_SIZE：控制文件下载分片
   - DEFAULT_FIELD_MAX_MEMORY_LIMIT：控制并行度

3. **不存在"达到内存限制就分片"的逻辑**
   - 即使128行占用1GB，仍是一个Chunk
   - 内存不足会OOM，而不是自动分片

4. **实际限制**
   - 硬限制：chunkRows配置
   - 软限制：可用内存（可能OOM）
   - 间接限制：文件大小、网络带宽

### 常见误解澄清

❌ **错误理解**：如果数据达到4MB或128MB，就会创建新的Chunk

✅ **正确理解**：只有达到chunkRows指定的行数才会创建新的Chunk

❌ **错误理解**：内存限制决定Chunk大小

✅ **正确理解**：内存限制决定并行加载的批次大小，不影响最终的Chunk划分