# Milvus Chunk实现详细分析

## 1. Chunk概述

Chunk是Milvus中的基础数据存储单元，用于在内存中高效地组织和访问列式数据。它提供了统一的接口来处理不同类型的数据，支持内存映射(mmap)和nullable字段。

### 1.1 设计理念

- **列式存储**：数据按列组织，提高缓存效率
- **零拷贝**：通过mmap直接映射文件到内存
- **类型安全**：为不同数据类型提供专门的实现
- **Nullable支持**：通过bitmap管理空值
- **内存优化**：支持内存和mmap两种存储模式

### 1.2 核心文件

- `internal/core/src/common/Chunk.h` - Chunk基类和子类定义
- `internal/core/src/common/ChunkWriter.h/cpp` - Chunk写入器
- `internal/core/src/common/ChunkTarget.h/cpp` - Chunk存储目标

## 2. Chunk基类设计

### 2.1 基类定义

```cpp
class Chunk {
public:
    Chunk(int64_t row_nums,
          char* data,
          uint64_t size,
          bool nullable,
          std::unique_ptr<MmapFileRAII> mmap_file_raii = nullptr);
    
    virtual ~Chunk() {
        munmap(data_, size_);  // 自动释放mmap内存
    }
    
    // 核心接口
    virtual const char* ValueAt(int64_t idx) const = 0;  // 纯虚函数
    virtual const char* Data() const { return data_; }
    
    // 属性访问
    uint64_t Size() const { return size_; }
    int64_t RowNums() const { return row_nums_; }
    const char* RawData() const { return data_; }
    
    // Nullable支持
    virtual bool isValid(int offset);

protected:
    char* data_;                              // 数据指针
    int64_t row_nums_;                        // 行数
    uint64_t size_;                           // 数据大小
    bool nullable_;                           // 是否支持null
    FixedVector<bool> valid_;                 // null bitmap解析结果
    std::unique_ptr<MmapFileRAII> mmap_file_raii_;  // mmap文件管理
};
```

### 2.2 数据布局

Chunk的数据布局遵循以下规则：
```
[Null Bitmap (if nullable)] [Actual Data]
```

- **Null Bitmap**：如果字段nullable，前(row_nums + 7) / 8字节存储null bitmap
- **Actual Data**：实际数据，布局取决于具体的Chunk子类

## 3. Chunk子类实现

### 3.1 FixedWidthChunk（固定宽度数据）

用于存储固定大小的数据类型，包括标量和定长向量。

```cpp
class FixedWidthChunk : public Chunk {
public:
    FixedWidthChunk(int32_t row_nums,
                    int32_t dim,           // 维度
                    char* data,
                    uint64_t size,
                    uint64_t element_size,  // 元素大小
                    bool nullable,
                    std::unique_ptr<MmapFileRAII> mmap_file_raii = nullptr);
    
    // 返回SpanBase用于批量访问
    milvus::SpanBase Span() const;
    
    // 获取指定索引的值
    const char* ValueAt(int64_t idx) const override {
        auto null_bitmap_bytes_num = (row_nums_ + 7) / 8;
        return data_ + null_bitmap_bytes_num + idx * element_size_ * dim_;
    }
    
    // 获取数据起始位置（跳过null bitmap）
    const char* Data() const override {
        auto null_bitmap_bytes_num = (row_nums_ + 7) / 8;
        return data_ + null_bitmap_bytes_num;
    }

private:
    int dim_;            // 维度（向量维度或1）
    int element_size_;   // 单个元素的字节大小
};
```

**适用数据类型**：
- 标量：Bool, Int8/16/32/64, Float, Double
- 向量：FloatVector, BinaryVector, Float16Vector, BFloat16Vector

**数据布局**：
```
[Null Bitmap] [Row0_Data] [Row1_Data] ... [RowN_Data]
每行数据大小 = element_size * dim
```

### 3.2 StringChunk（字符串数据）

用于存储变长字符串数据。

```cpp
class StringChunk : public Chunk {
public:
    StringChunk(int32_t row_nums,
                char* data,
                uint64_t size,
                bool nullable,
                std::unique_ptr<MmapFileRAII> mmap_file_raii = nullptr);
    
    // 通过下标访问字符串
    std::string_view operator[](const int i) const {
        return {data_ + offsets_[i], offsets_[i + 1] - offsets_[i]};
    }
    
    // 二分查找（用于有序主键）
    int binary_search_string(std::string_view target);
    
    // 批量获取string_view
    std::pair<std::vector<std::string_view>, FixedVector<bool>>
    StringViews(std::optional<std::pair<int64_t, int64_t>> offset_len);
    
    // 通过偏移量获取视图
    std::pair<std::vector<std::string_view>, FixedVector<bool>>
    ViewsByOffsets(const FixedVector<int32_t>& offsets);

protected:
    uint32_t* offsets_;  // 偏移量数组
};
```

**数据布局**：
```
[Null Bitmap] [Offsets Array] [String Data]
- Offsets Array: (row_nums + 1) * sizeof(uint32_t)
- String Data: 连续存储的字符串内容
```

**特点**：
- 使用偏移量数组定位每个字符串
- 返回string_view避免拷贝
- 支持二分查找（用于主键查询）

### 3.3 JSONChunk

```cpp
using JSONChunk = StringChunk;  // JSON复用StringChunk实现
```

JSON数据作为字符串存储，使用StringChunk的所有功能。

### 3.4 ArrayChunk（数组数据）

用于存储Array类型的字段。

```cpp
class ArrayChunk : public Chunk {
public:
    ArrayChunk(int32_t row_nums,
               char* data,
               uint64_t size,
               milvus::DataType element_type,  // 数组元素类型
               bool nullable,
               std::unique_ptr<MmapFileRAII> mmap_file_raii = nullptr);
    
    // 获取指定索引的数组视图
    ArrayView View(int idx) const {
        int idx_off = 2 * idx;
        auto offset = offsets_lens_[idx_off];      // 数据偏移
        auto len = offsets_lens_[idx_off + 1];     // 数组长度
        auto next_offset = offsets_lens_[idx_off + 2];
        
        // 处理字符串数组的特殊情况
        uint32_t offsets_bytes_len = 0;
        uint32_t* offsets_ptr = nullptr;
        if (IsStringDataType(element_type_)) {
            offsets_bytes_len = len * sizeof(uint32_t);
            offsets_ptr = reinterpret_cast<uint32_t*>(data_ + offset);
        }
        
        return ArrayView(data_ + offset + offsets_bytes_len,
                        len,
                        next_offset - offset - offsets_bytes_len,
                        element_type_,
                        offsets_ptr);
    }
    
    // 批量获取数组视图
    std::pair<std::vector<ArrayView>, FixedVector<bool>>
    Views(std::optional<std::pair<int64_t, int64_t>> offset_len = std::nullopt) const;

private:
    milvus::DataType element_type_;  // 元素类型
    uint32_t* offsets_lens_;         // 偏移量和长度数组
};
```

**数据布局**：
```
[Null Bitmap] [Offsets_Lens Array] [Array Data]
- Offsets_Lens Array: 每行2个uint32_t（offset, length）
- Array Data: 连续存储的数组数据
```

**特点**：
- 支持嵌套的字符串数组
- 每个数组可以有不同的长度
- 通过ArrayView提供零拷贝访问

### 3.5 SparseFloatVectorChunk（稀疏向量）

用于存储稀疏浮点向量。

```cpp
class SparseFloatVectorChunk : public Chunk {
public:
    SparseFloatVectorChunk(int32_t row_nums,
                          char* data,
                          uint64_t size,
                          bool nullable,
                          std::unique_ptr<MmapFileRAII> mmap_file_raii = nullptr);
    
    const char* Data() const override {
        return static_cast<const char*>(static_cast<const void*>(vec_.data()));
    }
    
    const char* ValueAt(int64_t i) const override {
        return static_cast<const char*>(
            static_cast<const void*>(vec_.data() + i));
    }
    
    // 获取向量数组（仅测试用）
    std::vector<knowhere::sparse::SparseRow<float>>& Vec() {
        return vec_;
    }
    
    int64_t Dim() { return dim_; }

private:
    int64_t dim_ = 0;  // 最大维度
    std::vector<knowhere::sparse::SparseRow<float>> vec_;  // 稀疏向量数组
};
```

**数据布局**：
```
[Null Bitmap] [Offsets Array] [Sparse Data]
- Offsets Array: (row_nums + 1) * sizeof(uint64_t)
- Sparse Data: 每个稀疏向量的(index, value)对
```

**特点**：
- 使用Knowhere的SparseRow表示
- 只存储非零元素
- 动态计算最大维度

## 4. ChunkWriter和ChunkTarget

### 4.1 ChunkWriter架构

```cpp
class ChunkWriterBase {
public:
    // 写入Arrow RecordBatch数据
    virtual void write(std::shared_ptr<arrow::RecordBatchReader> data) = 0;
    
    // 完成写入，返回Chunk
    virtual std::shared_ptr<Chunk> finish() = 0;
    
protected:
    int row_nums_ = 0;
    File* file_ = nullptr;           // 可选的mmap文件
    size_t file_offset_ = 0;         // 文件偏移
    bool nullable_ = false;          // 是否支持null
    std::shared_ptr<ChunkTarget> target_;  // 存储目标
};
```

### 4.2 ChunkTarget存储目标

**ChunkTarget接口**：
```cpp
class ChunkTarget {
public:
    virtual void write(const void* data, size_t size, bool append = true) = 0;
    virtual void skip(size_t size) = 0;
    virtual void seek(size_t offset) = 0;
    virtual std::pair<char*, size_t> get() = 0;
    virtual size_t tell() = 0;
};
```

**两种实现**：

1. **MemChunkTarget**（内存模式）：
```cpp
class MemChunkTarget : public ChunkTarget {
public:
    MemChunkTarget(size_t cap) {
        // 使用匿名mmap分配内存
        auto m = mmap(nullptr, cap, 
                     PROT_READ | PROT_WRITE,
                     MAP_PRIVATE | MAP_ANON, -1, 0);
        data_ = reinterpret_cast<char*>(m);
    }
};
```

2. **MmapChunkTarget**（文件mmap模式）：
```cpp
class MmapChunkTarget : public ChunkTarget {
    struct Buffer {
        char buf[1 << 14];  // 16KB缓冲区
        size_t pos = 0;
        
        void write(const void* data, size_t size);
        void flush();  // 刷新到文件
    };
    
private:
    File& file_;
    Buffer buffer_;  // 写缓冲区
};
```

## 5. Chunk创建工厂

### 5.1 create_chunk函数

```cpp
std::shared_ptr<Chunk> create_chunk(
    const FieldMeta& field_meta,
    int dim,
    std::shared_ptr<arrow::RecordBatchReader> r)
{
    std::shared_ptr<ChunkWriterBase> w;
    bool nullable = field_meta.is_nullable();
    
    switch (field_meta.get_data_type()) {
        case DataType::BOOL:
            w = std::make_shared<ChunkWriter<arrow::BooleanArray, bool>>(
                dim, nullable);
            break;
        case DataType::INT8:
            w = std::make_shared<ChunkWriter<arrow::Int8Array, int8_t>>(
                dim, nullable);
            break;
        // ... 其他类型
        case DataType::STRING:
        case DataType::VARCHAR:
            w = std::make_shared<StringChunkWriter>(nullable);
            break;
        case DataType::ARRAY:
            w = std::make_shared<ArrayChunkWriter>(
                field_meta.get_element_type(), nullable);
            break;
        case DataType::VECTOR_SPARSE_FLOAT:
            w = std::make_shared<SparseFloatVectorChunkWriter>(nullable);
            break;
    }
    
    w->write(std::move(r));
    return w->finish();
}
```

### 5.2 数据类型映射

| DataType | Chunk类型 | ChunkWriter |
|----------|-----------|-------------|
| BOOL, INT8/16/32/64, FLOAT, DOUBLE | FixedWidthChunk | ChunkWriter<ArrowType, T> |
| VECTOR_FLOAT/BINARY/FLOAT16/BFLOAT16 | FixedWidthChunk | ChunkWriter<FixedSizeBinaryArray, T> |
| STRING, VARCHAR | StringChunk | StringChunkWriter |
| JSON | JSONChunk | JSONChunkWriter |
| ARRAY | ArrayChunk | ArrayChunkWriter |
| VECTOR_SPARSE_FLOAT | SparseFloatVectorChunk | SparseFloatVectorChunkWriter |

## 6. 内存管理机制

### 6.1 RAII管理

```cpp
struct MmapFileRAII {
    std::string file_path;
    ~MmapFileRAII() {
        // 自动删除临时文件
        std::filesystem::remove(file_path);
    }
};
```

### 6.2 内存分配策略

1. **内存模式**：
   - 使用匿名mmap分配内存
   - 由操作系统管理页面
   - Chunk析构时自动munmap

2. **文件mmap模式**：
   - 数据写入文件
   - mmap映射文件到内存
   - 支持大于物理内存的数据

### 6.3 零拷贝设计

- 直接返回指针和视图
- 避免数据复制
- 延迟计算和解析

## 7. 使用场景

### 7.1 数据加载

```cpp
// 从远程存储加载
auto field_data = DownloadAndDecodeRemoteFile(cm, filepath);

// 创建Chunk
auto chunk = create_chunk(field_meta, dim, field_data->GetReader());

// 使用mmap模式
auto file = File::Open(path, O_CREAT | O_TRUNC | O_RDWR);
auto chunk = create_chunk(field_meta, dim, file, 0, reader);
```

### 7.2 数据访问

```cpp
// 固定宽度数据
auto fixed_chunk = std::dynamic_pointer_cast<FixedWidthChunk>(chunk);
auto span = fixed_chunk->Span();
for (int i = 0; i < span.row_count(); i++) {
    auto value = span.get_data<float>(i);
}

// 字符串数据
auto str_chunk = std::dynamic_pointer_cast<StringChunk>(chunk);
for (int i = 0; i < str_chunk->RowNums(); i++) {
    auto str_view = (*str_chunk)[i];
}

// 数组数据
auto arr_chunk = std::dynamic_pointer_cast<ArrayChunk>(chunk);
auto [views, valid] = arr_chunk->Views();
for (auto& view : views) {
    // 处理数组视图
}
```

## 8. 性能优化

### 8.1 内存局部性
- 列式存储提高缓存命中率
- 连续内存布局优化SIMD操作

### 8.2 延迟计算
- Null bitmap按需解析
- 稀疏向量维度动态计算

### 8.3 批量操作
- 通过Span/View批量访问
- 减少虚函数调用开销

### 8.4 mmap优化
- 大数据使用文件mmap
- 利用操作系统页面缓存
- 支持超内存数据处理

## 9. 总结

Chunk是Milvus存储层的核心组件，通过精心设计的类层次结构支持各种数据类型：

1. **统一接口**：基类提供统一的访问接口
2. **类型特化**：每种数据类型有优化的实现
3. **内存高效**：支持mmap和零拷贝
4. **灵活扩展**：易于添加新的数据类型
5. **性能优化**：列式存储和批量操作

这种设计使得Milvus能够高效地处理各种类型的向量和标量数据，为上层的查询和索引操作提供了坚实的基础。