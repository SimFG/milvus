# Milvus 表达式系统扩展指南

## 1. 表达式定义和读取机制

### 1.1 表达式定义体系

Milvus的表达式系统基于ANTLR (Another Tool for Language Recognition) 构建，采用分层架构：

1. **语法定义层** (`Plan.g4`)
   - 位置：`/internal/parser/planparserv2/Plan.g4`
   - 定义了表达式的语法规则
   - 使用ANTLR v4语法格式

2. **解析器层** 
   - ANTLR自动生成的解析器代码
   - 位置：`/internal/parser/planparserv2/generated/`
   - 包括词法分析器(Lexer)和语法分析器(Parser)

3. **访问器层** (`parser_visitor.go`)
   - 位置：`/internal/parser/planparserv2/parser_visitor.go`
   - 实现Visitor模式，遍历AST并生成执行计划

4. **执行层** (C++实现)
   - 位置：`/internal/core/src/exec/expression/`
   - 包含各种表达式执行器

### 1.2 表达式读取流程

```
用户输入表达式字符串
    ↓
ANTLR Lexer (词法分析)
    ↓
ANTLR Parser (语法分析，生成AST)
    ↓
ParserVisitor (遍历AST，生成PlanNode)
    ↓
C++ Executor (执行表达式)
```

### 1.3 当前支持的表达式类型

从 `Plan.g4` 文件可以看到，当前支持以下表达式类型：

**基础表达式**：
- 常量：整数、浮点数、布尔值、字符串
- 标识符：字段引用
- 数组：`[expr, expr, ...]`

**运算符表达式**：
- 算术：`+`, `-`, `*`, `/`, `%`, `**`
- 比较：`<`, `<=`, `>`, `>=`, `==`, `!=`
- 逻辑：`&&`, `||`, `!`
- 位运算：`&`, `|`, `^`, `~`, `<<`, `>>`

**特殊表达式**：
- IN操作：`expr IN expr`
- LIKE操作：`expr LIKE pattern`
- EXISTS操作：`EXISTS expr`
- NULL检查：`IS NULL`, `IS NOT NULL`
- 范围查询：`expr1 < field < expr2`

**函数表达式**：
- 内置函数：`text_match()`, `json_contains()`, `array_length()` 等
- 通用函数调用：`function_name(args...)`

## 2. 函数扩展机制

### 2.1 函数类型分类

Milvus中的函数分为两类：

1. **特殊函数**：在语法层面特殊处理
   - 如：`text_match`, `json_contains`, `array_length`
   - 在 `Plan.g4` 中有专门的语法规则
   - 在 `parser_visitor.go` 中有专门的Visit方法

2. **通用函数**：通过Call表达式处理
   - 通过 `Identifier '(' args ')'` 语法规则匹配
   - 由 `VisitCall` 方法统一处理
   - 在C++层通过 `FunctionFactory` 注册和调用

### 2.2 函数注册机制

函数通过 `FunctionFactory` 注册：

```cpp
// 位置：/internal/core/src/exec/expression/function/FunctionFactory.h
class FunctionFactory {
    void RegisterFilterFunction(
        std::string func_name,
        std::vector<DataType> func_param_type_list,
        FilterFunctionPtr func
    );
};
```

注册示例（`FunctionFactory.cpp`）：
```cpp
RegisterFilterFunction("starts_with",
                      {DataType::VARCHAR, DataType::VARCHAR},
                      function::StartsWithVarchar);
```

## 3. 添加新函数的步骤

根据函数类型的不同，有两种添加路径：

### 方案A：添加通用函数（推荐）

这种方式不需要修改语法文件，更简单。

#### 步骤1：实现函数逻辑

创建函数实现文件：
```
位置：/internal/core/src/exec/expression/function/impl/YourFunction.cpp
```

包含以下内容：
- 函数实现（参考 `StartsWith.cpp`）
- 参数验证
- 结果计算

#### 步骤2：注册函数

在 `FunctionFactory::RegisterAllFunctions()` 中添加：
```
位置：/internal/core/src/exec/expression/function/FunctionFactory.cpp
```

注册代码：
```cpp
RegisterFilterFunction("your_function_name",
                      {参数1类型, 参数2类型, ...},
                      function::YourFunctionImpl);
```

#### 步骤3：编译和测试

- 重新编译C++代码
- 编写单元测试
- 测试函数调用：`your_function_name(arg1, arg2)`

### 方案B：添加特殊函数（复杂）

当函数需要特殊语法或特殊处理时使用。

#### 步骤1：修改语法文件

编辑 `Plan.g4`：
```
位置：/internal/parser/planparserv2/Plan.g4
```

添加新的语法规则：
```antlr
expr:
    ...
    | YourFunction'('expr',' expr')'  # YourFunction
    ...

YourFunction: 'your_function' | 'YOUR_FUNCTION';
```

#### 步骤2：重新生成解析器

运行ANTLR生成新的解析器代码：
```bash
cd internal/parser/planparserv2
make generate-grammar
```

#### 步骤3：实现Visitor方法

在 `parser_visitor.go` 中添加：
```
位置：/internal/parser/planparserv2/parser_visitor.go
```

实现Visit方法：
```go
func (v *ParserVisitor) VisitYourFunction(ctx *parser.YourFunctionContext) interface{} {
    // 1. 获取参数
    // 2. 验证参数类型
    // 3. 构建表达式节点
    // 4. 返回ExprWithType
}
```

#### 步骤4：添加C++执行器

创建执行器类：
```
位置：/internal/core/src/exec/expression/YourFunctionExpr.h/.cpp
```

实现以下内容：
- 继承自 `Expr` 基类
- 实现 `Eval()` 方法
- 处理批量数据

#### 步骤5：在执行器工厂中注册

修改执行器创建逻辑，识别新的表达式类型。

#### 步骤6：编译和测试

- 重新编译Go和C++代码
- 编写集成测试
- 验证端到端功能

## 4. 最佳实践建议

### 4.1 选择合适的方案

- **优先使用通用函数**：简单、维护成本低
- **特殊函数仅在必要时使用**：
  - 需要特殊语法
  - 需要编译时优化
  - 需要特殊的参数处理

### 4.2 函数命名规范

- 使用小写字母和下划线：`function_name`
- 支持大小写变体：`function_name` 和 `FUNCTION_NAME`
- 名称应清晰表达功能

### 4.3 参数类型支持

确保函数支持必要的数据类型：
- 标量类型：INT64, FLOAT, VARCHAR, BOOL
- 复杂类型：JSON, ARRAY
- 考虑NULL值处理

### 4.4 性能考虑

- 使用批量处理（向量化执行）
- 避免逐行处理
- 利用SIMD指令加速（如适用）
- 实现早期终止优化

### 4.5 测试要求

- 单元测试：测试函数逻辑
- 集成测试：测试端到端流程
- 性能测试：验证批量处理性能
- 边界条件：NULL值、空数组、特殊字符等

## 5. 示例：添加 `ends_with` 函数

以下是添加一个简单的 `ends_with` 字符串函数的完整步骤：

### 使用通用函数方案

#### 1. 创建函数实现
文件：`/internal/core/src/exec/expression/function/impl/EndsWith.cpp`

```cpp
void EndsWithVarchar(const RowVector& args, FilterFunctionReturn& result) {
    // 验证参数个数
    if (args.childrens().size() != 2) {
        PanicInfo(ExprInvalid, "invalid argument count");
    }
    
    // 获取参数
    auto strs = std::dynamic_pointer_cast<SimpleVector>(args.child(0));
    auto suffixes = std::dynamic_pointer_cast<SimpleVector>(args.child(1));
    
    // 类型检查
    CheckVarcharOrStringType(strs);
    CheckVarcharOrStringType(suffixes);
    
    // 计算结果
    TargetBitmap bitmap(strs->size(), false);
    TargetBitmap valid_bitmap(strs->size(), true);
    
    for (size_t i = 0; i < strs->size(); ++i) {
        if (strs->ValidAt(i) && suffixes->ValidAt(i)) {
            auto* str = reinterpret_cast<std::string*>(
                strs->RawValueAt(i, sizeof(std::string)));
            auto* suffix = reinterpret_cast<std::string*>(
                suffixes->RawValueAt(i, sizeof(std::string)));
            
            // 检查是否以suffix结尾
            if (str->size() >= suffix->size()) {
                bitmap.set(i, str->compare(
                    str->size() - suffix->size(), 
                    suffix->size(), 
                    *suffix) == 0);
            }
        } else {
            valid_bitmap[i] = false;
        }
    }
    
    result = std::make_shared<ColumnVector>(
        std::move(bitmap), std::move(valid_bitmap));
}
```

#### 2. 注册函数
在 `FunctionFactory.cpp` 的 `RegisterAllFunctions()` 中添加：

```cpp
RegisterFilterFunction("ends_with",
                      {DataType::VARCHAR, DataType::VARCHAR},
                      function::EndsWithVarchar);
```

#### 3. 编译测试
```bash
# 编译
cd internal/core
mkdir build && cd build
cmake ..
make -j

# 使用
# 在查询表达式中使用：ends_with(field_name, "suffix")
```

## 6. 常见问题

### Q1: 如何处理可变参数函数？
A: 在 `VisitCall` 中动态处理参数列表，或为不同参数个数注册多个函数签名。

### Q2: 如何支持多种数据类型？
A: 为每种类型组合注册不同的函数实现，如 `starts_with_varchar`, `starts_with_string`。

### Q3: 如何实现聚合函数？
A: 聚合函数需要特殊处理，通常在查询计划的不同阶段执行，需要修改执行引擎。

### Q4: 如何调试新添加的函数？
A: 
- 在Go层添加日志输出AST结构
- 在C++层使用gdb调试执行过程
- 编写详细的单元测试

### Q5: 函数名冲突如何处理？
A: FunctionFactory使用函数名+参数类型作为key，相同名称但不同参数类型的函数可以共存（重载）。

## 7. 总结

Milvus的表达式系统提供了灵活的扩展机制：

1. **通用函数机制**简单易用，适合大多数场景
2. **特殊函数机制**功能强大，适合复杂需求
3. 完整的函数生命周期：定义→解析→执行
4. 清晰的分层架构便于维护和扩展

选择合适的扩展方式，遵循最佳实践，可以高效地为Milvus添加新的表达式功能。