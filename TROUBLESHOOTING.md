# 故障排除指南

## 403 Access Denied 错误

### 问题描述
在使用一段时间后,服务突然开始返回 `403 Access Denied` 错误:
```
ERRO[0131] Cursor API returned non-OK status             status_code=403
ERRO[0131] Failed to create chat completion              error="{\"error\":\"Access denied\"}"
```

### 原因分析
1. **Token 过期**: `x-is-human` token 缓存时间过长,导致 token 失效
2. **频率限制**: 短时间内发送过多请求触发了 Cursor API 的速率限制
3. **重复 Token**: 使用相同的 token 进行多次请求被识别为异常行为

### 解决方案

#### 1. 已实施的自动修复
最新版本已经包含以下改进:

- **动态浏览器指纹**: 每次请求使用真实且随机的浏览器指纹信息
  - 根据操作系统自动选择合适的平台配置 (Windows/macOS/Linux)
  - 随机 Chrome 版本 (120-130)
  - 随机语言设置和 Referer
  - 真实的 User-Agent 和 sec-ch-ua headers
- **缩短缓存时间**: 将 `x-is-human` token 缓存时间从 30 分钟缩短到 1 分钟
- **自动重试机制**: 遇到 403 错误时自动清除缓存并重试(最多 2 次)
- **指纹刷新**: 403 错误时自动刷新浏览器指纹配置
- **错误恢复**: 失败时自动清除缓存,确保下次请求使用新 token
- **指数退避**: 重试时使用递增的等待时间

#### 2. 手动解决步骤
如果问题持续存在:

1. **重启服务**:
   ```bash
   # 停止当前服务 (Ctrl+C)
   # 重新启动
   ./cursor2api-go
   ```

2. **检查日志**:
   查看是否有以下日志:
   - `Received 403 Access Denied, clearing token cache and retrying...` - 自动重试
   - `Failed to fetch x-is-human token` - Token 获取失败
   - `Fetched x-is-human token` - Token 获取成功

3. **等待冷却期**:
   如果频繁遇到 403 错误,建议等待 5-10 分钟后再使用

4. **检查网络**:
   确保能够访问 `https://cursor.com`

#### 3. 预防措施

1. **控制请求频率**: 避免在短时间内发送大量请求
2. **监控日志**: 注意 `x-is-human token` 的获取频率
3. **合理配置超时**: 在 `.env` 文件中设置合理的超时时间

### 配置建议

在 `.env` 文件中:
```bash
TIMEOUT=120  # 增加超时时间,避免频繁重试
MAX_INPUT_LENGTH=100000  # 限制输入长度,减少请求大小
```

### 调试模式

如果需要查看详细的调试信息,可以启用调试模式:
```bash
# 方式 1: 修改 .env 文件
DEBUG=true

# 方式 2: 使用环境变量
DEBUG=true ./cursor2api-go
```

这将显示:
- 每次请求的 `x-is-human` token (前 50 字符)
- 请求的 payload 大小
- 重试次数
- 详细的错误信息

## 其他常见问题

### Cloudflare 403 错误
如果看到 `Cloudflare 403` 错误,说明请求被 Cloudflare 防火墙拦截。这通常是因为:
- IP 被标记为可疑
- User-Agent 不匹配
- 缺少必要的浏览器指纹

**解决方案**: 检查 `.env` 文件中的浏览器指纹配置（`USER_AGENT`、`UNMASKED_VENDOR_WEBGL`、`UNMASKED_RENDERER_WEBGL`）是否正确。

### 连接超时
如果频繁出现连接超时:
1. 检查网络连接
2. 增加 `.env` 文件中的 `TIMEOUT` 配置值
3. 检查防火墙设置

### Token 获取失败
如果无法获取 `x-is-human` token:
1. 检查 `.env` 文件中的 `SCRIPT_URL` 配置是否正确
2. 确保 `jscode/main.js` 和 `jscode/env.js` 文件存在
3. 检查 Node.js 环境是否正常安装（Node.js 18+）

## 联系支持

如果问题仍未解决,请提供以下信息:
1. 完整的错误日志
2. `.env` 文件配置（隐藏敏感信息如 `API_KEY`）
3. 使用的 Go 版本和 Node.js 版本
4. 操作系统信息



### SCRIPT_URL获取指令

```
指令内容： “请启动浏览器 Subagent 执行 Deep Fingerprint Detection (深度特征探测)。

任务目标：

获取 SCRIPT_URL：模拟访问 cursor.com/login，在所有动态加载的 /assets/*.js 中寻找包含 checksum、x-is-human 或 web_selection 核心特征的混淆脚本，锁定最新的指纹算法源路径。
获取版本号：通过模拟指纹嗅探，提取当前官方 IDE 的最新有效版本号（即 x-cursor-client-version）。
执行动作：

探测完成后，请直接告诉我最新的 SCRIPT_URL。
请自动帮助我更新 .env 文件中的 SCRIPT_URL。
请自动将 

utils/headers.go
 中的 x-cursor-client-version 修改为探测到的最新版本。
最后重新编译程序：go build -o cursor2api-go.exe。”
```

指令背后的技术逻辑说明（供您参考）：
为什么要访问 /login？ 因为 Cursor 的主页是静态展示，只有登录页和设置页才会加载包含复杂校验算法的正规业务脚本。
为什么要扫描 checksum？ 这是指纹算法的关键字，只要搜到它，就意味着找到了“通行证工厂”的源头。
版本号为什么要强制跟随？ 因为新版的 JS 脚本往往包含针对旧版本的“弃用”检查，如果不配合更新 Header 里的版本号，即便指纹算对了，也会因为版本过低被降级处理。


获取方法通常是：

打开浏览器，访问 Cursor 的任意页面（如 https://cursor.com/）。
查看网页源代码，或者在 Network 标签里按 Ctrl+F 搜 getFingerprint。
找到对应的 .js 文件（通常是一个名字很乱的混淆后的文件），下载它。
将下载的内容替换到本项目目录下的 

jscode/main.js
。


一次ai获取的经历：

实地探测结果出来了！我模拟浏览器访问了 https://cursor.com 及其文档页，成功定位到了目前负责指纹生成的脚本。

📡 探测结果：最新的指纹脚本 URL
目前官网上最核心的两个脚本地址如下（当前日期 2026-03-29）：

主入口脚本 (最推荐作为 SCRIPT_URL): https://cursor.com/docs-static/_next/static/chunks/0jf6~fkj7siwy.js?dpl=dpl_AYjBkEAgHoxjQoRpowczw7bHoiNm

理由：这个脚本带有 id="_R_" 标识，是加载所有业务逻辑的“总闸”。
核心算法脚本 (体积最大，包含 WASM): https://cursor.com/docs-static/_next/static/chunks/180ai4wjrjlz8.js?dpl=dpl_AYjBkEAgHoxjQoRpowczw7bHoiNm

理由：体积约 330KB，包含了复杂的混淆代码和 WebAssembly 引用，通常真正的 x-is-human 校验算法就藏在这里。
🔍 我是怎么找到它们的？（您可以照着做）
如果您以后发现指纹失效了，可以按照以下步骤手动抓取：

访问受控页面：打开 https://cursor.com/learn/how-ai-models-work（这个页面比主页干扰少，更稳定）。
右键 -> 查看网页源代码：
搜索关键词 id="_R_"。
找到对应的 <script src="...">，那个 src 里的地址就是最新的入口脚本。
开发者工具 (F12) -> Network (网络)：
刷新页面，在 JS 过滤器中寻找后缀带 ?dpl=dpl_... 的文件。
通常体积在 300KB 以上的那个文件就是包含指纹算法的核心文件。
注意 dpl 参数：URL 末尾的 dpl_AYjBkEAg... 是 Cursor 的部署版本号，每次官网更新代码这个动态后缀都会变。
建议：您可以把第一个 URL 更新到您的 

.env
 的 SCRIPT_URL 中。虽然目前后端代码主要读本地的 

jscode/main.js
，但保持该 URL 的正确性有助于您之后（或使用其他工具时）快速追踪到官网的最新变化。