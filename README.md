# KotoType

轻量级 Windows 原生语音听写工具。采用纯原生 Direct2D / DirectWrite Fluent 界面（支持 Windows 11 Mica 云母与亮/暗主题自适应）和 Windows TSF 输入法服务（不切换键盘输入法，精准写入任意输入框并自动跟随光标换行）。

主程序本身**不包含**任何第三方语音识别协议，而是通过统一的外部模块协议从 `providers/` 目录动态加载语音识别模块。

## 特性

- **原生轻量**：零第三方 UI 框架、零 CGO，主程序单文件仅约 3.3 MB，待机自动释放渲染目标与闲置内存。
- **TSF 语音输入法服务**：内嵌无 CRT 的 10 KB TSF TIP 服务，支持免重启热更新，在浏览器、微信、VS Code、记事本等任意应用中精准定位光标并写入文字。
- **可插拔识别模块**：扫描程序同目录或配置目录下的 `providers/kototype-provider-*.exe`，动态渲染模块专属配置项。
- **便携/系统配置切换**：支持在 `%AppData%\kototype\config.json` 与程序同目录 `config.json` 之间一键迁移。

## 外部识别模块协议

将任意语言编写的模块可执行文件命名为 `kototype-provider-<id>.exe` 并放入 `providers/` 目录即可自动加载。

### 1. 元数据声明：`--manifest`

主程序调用 `<plugin> --manifest`，模块向 `stdout` 输出单行 JSON 并退出码 0：

```json
{
  "id": "demo",
  "name": "示例语音引擎",
  "version": "1.0.0",
  "fields": [
    {
      "key": "language",
      "label": "语言",
      "type": "select",
      "choices": [
        {"value": "", "name": "默认"},
        {"value": "en", "name": "英语"}
      ]
    },
    {
      "key": "domain",
      "label": "领域优化",
      "type": "select",
      "choices": [{"value": "", "name": "通用"}],
      "when": {"key": "language", "value": ""}
    }
  ]
}
```

### 2. 流式识别：`--recognize`

主程序调用 `<plugin> --recognize`：

- **`stdin`（主程序 → 模块）**：
  1. 首行为 UTF-8 JSON 请求（以 `\n` 结尾）：`{"options":{"language":""},"end_pause_ms":0}`
  2. 随后持续写入原始 `16 kHz / 单声道 / 16-bit LE PCM` 音频流，用户结束说话时关闭 `stdin`（EOF）。
- **`stdout`（模块 → 主程序）**：
  - 每行输出一个 JSON 事件：
    - 增量/回改结果：`{"text":"当前完整文本","stable":12}`（`stable` 为已确认不再回改的字节偏移）
    - 静音超时：`{"silence":true}`
    - 报错结束：`{"error":"错误信息"}`

## 构建

```powershell
# 1. 如修改了 tip/kototype_tip.cpp，先编译 TSF 输入法服务 DLL（需要 MSVC）
cmd /c tip\build.cmd

# 2. 如修改了 assets/logo.svg，重新生成图标与 Windows 资源
go generate ./cmd/kototype

# 3. 编译主程序
$env:CGO_ENABLED="0"; $env:GOOS="windows"; $env:GOARCH="amd64"
go build -trimpath -ldflags "-s -w -X main.version=v0.1.0 -H windowsgui" -o dist/kototype.exe ./cmd/kototype
```
