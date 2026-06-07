# WebRTC Remote Desktop White Screen Runbook

本文档用于排查与修复 `xworkmate-bridge` 的 WebRTC 远程桌面频发白屏问题，覆盖编码、RTP、WebRTC 协商、远端部署验证和回滚流程。

适用范围：

- Bridge 仓库：`/Users/shenlan/workspaces/ai-workspace-lab/xworkmate-bridge`
- 远端主机：`ubuntu@xworkmate-bridge.svc.plus`
- user service：`xworkmate-bridge.service`
- APP 侧入口：`xworkmate-app` 的 Remote Desktop 面板

## 目标

- 把“已连接但无画面 / 长时间等待首帧”拆解到编码、RTP、WebRTC、前端显示四层。
- 让 Bridge 输出 browser-friendly H.264：
  - `baseline` / `constrained-baseline`
  - `yuv420p` / `I420` / 4:2:0
  - `zerolatency`
  - `key-int-max=30`
  - 定期 SPS / PPS
- 在白屏现场拿到可判定证据，而不是只看“connected”状态。

## 症状定义

- APP 显示 `已连接`，但视频区域白屏。
- APP 显示 `WebRTC 已连接，正在等待远程桌面首帧...`，长时间不消失。
- 首次连接偶发成功，断开重连后更容易白屏。

## 前置检查：先排除账号同步 / 鉴权问题

如果 APP 还没进入 WebRTC 连接状态，不要直接按白屏处理。下面这些信号说明请求尚未进入远程桌面 offer / RTP 链路：

- APP 远程桌面面板显示 `已断开`，文案为 `未开启 AI 工作空间流。点击“连接AI工作空间”启动视频流。`
- APP 账号同步显示 `账号同步状态：失败`
- APP 同步说明显示 `Bridge token expired or rejected. Please re-sync the account token.`
- APP 关于页显示 Bridge runtime `Status: unauthorized`
- Bridge 日志里点击 APP 后没有新的 `Starting Remote Desktop session`、`xworkmate.desktop.offer` 或 `WebRTC RTP stats`

这类现场的首要结论是：App 侧 managed bridge token 已过期、被拒绝或未重新同步。此时 bridge 服务可能已经是最新版本且运行正常，但 APP 没有可用凭据调用受保护接口。

2026-06-07 现场确认过一个容易漏掉的变体：Go bridge origin 已接受 `BRIDGE_REVIEW_AUTH_TOKEN`，但 Caddy 公网入口只放行主 `BRIDGE_AUTH_TOKEN`，导致 `review@svc.plus` 走公网 `/api/ping` 和 `/acp/rpc` 返回 `401`。这同样表现为 APP token 被拒绝，且不会进入 WebRTC/RTP 层。

处理步骤：

1. 在 APP 设置页执行 `重新同步`。
2. 同步成功后刷新版本信息，确认 Bridge runtime 不再是 `unauthorized`。
3. 如果账号是 `review@svc.plus` 或使用 review/beta token，确认公网 Caddy 入口也放行 review token。
4. 再回到远程桌面面板点击连接，进入 WebRTC / RTP 排查。

远端校验方式：

```bash
ssh ubuntu@xworkmate-bridge.svc.plus '
  curl -sS -o /dev/null -w "%{http_code}\n" https://xworkmate-bridge.svc.plus/api/ping
'
```

无 token 返回 `401` 是预期结果；不要把它误判为部署失败。要确认服务端版本，需要使用 user service 环境里的 token，且不要把 token 打印到日志或文档：

```bash
ssh ubuntu@xworkmate-bridge.svc.plus '
  TOKEN=$(systemctl --user show -p Environment --value xworkmate-bridge.service |
    tr " " "\n" |
    sed -n "s/^BRIDGE_AUTH_TOKEN=//p")
  curl -sS -H "Authorization: Bearer ${TOKEN}" https://xworkmate-bridge.svc.plus/api/ping
  unset TOKEN
'
```

期望看到 `status=ok`，并且 `commit` 等于最新部署 commit。

如果配置了 `BRIDGE_REVIEW_AUTH_TOKEN`，必须额外验证公网入口也接受 review token：

```bash
ssh ubuntu@xworkmate-bridge.svc.plus '
  TOKEN=$(systemctl --user show -p Environment --value xworkmate-bridge.service |
    tr " " "\n" |
    sed -n "s/^BRIDGE_REVIEW_AUTH_TOKEN=//p")
  if [ -n "${TOKEN}" ]; then
    curl -sS -o /dev/null -w "%{http_code}\n" \
      -H "Authorization: Bearer ${TOKEN}" \
      https://xworkmate-bridge.svc.plus/api/ping
  fi
  unset TOKEN
'
```

期望返回 `200`。如果本机 `127.0.0.1:8787` 返回 `200` 但公网 HTTPS 返回 `401`，问题在 Caddy / ingress token allowlist，不在 WebRTC。

## 根因判断速查

### 1. ICE connected，但 Bridge RTP 包不增长

判断：

- Bridge 日志中 `WebRTC RTP stats: packets=0`
- 或 `Capture pipeline exited with error`

说明：

- 问题在 Bridge capture / encoder / 本机 RTP 发送侧，不是公网网络抖动。

### 2. RTP 包增长，但 APP `packetsReceived` 不增长

判断：

- Bridge 端 `packets`、`bytes` 持续增长
- APP 侧 inbound stats 里 `packetsReceived` 不增长

说明：

- 问题在 ICE candidate、NAT、TURN、链路可达性或浏览器收包侧。

### 3. `packetsReceived` 增长，但 `framesDecoded` 不增长

判断：

- APP 侧 inbound video stats 里 `packetsReceived > 0`
- `framesDecoded == 0`

说明：

- 问题高度集中在 H.264 profile / pixel format / SPS/PPS / 解码兼容。

### 4. `framesDecoded` 增长，但仍白屏

判断：

- APP 侧 stats 能看到 decoded frames
- UI 仍然空白

说明：

- 问题在 Flutter renderer / track attach / view lifecycle / stale stream。

## 本次修复结论

本次真实根因不是单纯网络延迟，而是两段串联问题：

1. 旧 GStreamer pipeline 输出 `high-4:4:4` H.264，`profile-level-id=f40020`，对 WebRTC / browser 解码不友好。
2. 修成 `I420` 后暴露远端桌面真实尺寸 `1352x847`，高度为奇数，4:2:0 编码链路无法稳定工作，pipeline 直接退出，导致 RTP 始终为 0。

本次二次排查还发现一个入口层问题：

3. Caddy 公网入口曾只放行主 `BRIDGE_AUTH_TOKEN`，未放行 user service 中的 `BRIDGE_REVIEW_AUTH_TOKEN`。因此 `review@svc.plus` 会看到 `Bridge token expired or rejected`，并且无法发起 `xworkmate.desktop.offer`。

修复后的稳定策略：

- 强制把 capture 输出转换到 `I420`
- 强制缩放到偶数分辨率
- H.264 限制为 `baseline`
- `rtph264pay config-interval=1`
- `x264enc` 启用 `zerolatency`
- Bridge 定期输出 RTP stats
- APP 在等待首帧时输出 inbound video stats
- Caddy 公网入口同时放行主 token 与 review token，并验证无 token 仍为 `401`
- 只保留 user service 作为当前 bridge origin，避免 system service 与 user service 抢占 `127.0.0.1:8787`

注意：如果 APP 当前显示 `Bridge token expired or rejected` 或 Bridge runtime `unauthorized`，这是鉴权前置问题，不是本次 H.264 / RTP 白屏根因的复发。必须先重新同步账号 token，再验证 WebRTC 链路。

## 代码落点

Bridge：

- [internal/desktop/pipeline.go](/Users/shenlan/workspaces/ai-workspace-lab/xworkmate-bridge/internal/desktop/pipeline.go)
- [internal/desktop/webrtc.go](/Users/shenlan/workspaces/ai-workspace-lab/xworkmate-bridge/internal/desktop/webrtc.go)
- [internal/desktop/pipeline_test.go](/Users/shenlan/workspaces/ai-workspace-lab/xworkmate-bridge/internal/desktop/pipeline_test.go)

APP 诊断：

- `/Users/shenlan/workspaces/ai-workspace-lab/xworkmate-app/lib/features/desktop/desktop_client.dart`
- `/Users/shenlan/workspaces/ai-workspace-lab/xworkmate-app/lib/features/desktop/desktop_view.dart`
- `/Users/shenlan/workspaces/ai-workspace-lab/xworkmate-app/test/features/desktop/desktop_client_test.dart`

## 期望日志

健康的编码与 RTP 发送链路应该出现下面这类信号：

```text
Starting capture pipeline: gst-launch-1.0 ... videoconvert ! videoscale ! video/x-raw,format=I420,width=1280,height=720,framerate=30/1 ! x264enc ... tune=zerolatency ... key-int-max=30 ! video/x-h264,profile=baseline ! rtph264pay config-interval=1 pt=96
... GstVideoScale: caps = video/x-raw, width=(int)1280, height=(int)720, format=(string)I420
... GstX264Enc: caps = video/x-h264 ... profile=(string)baseline
... GstRtpH264Pay: caps = application/x-rtp ... profile-level-id=(string)42c01f, profile=(string)constrained-baseline
WebRTC RTP stats: packets=1471 bytes=1236907 packetDelta=1471 byteDelta=1236907 writeErrors=0
```

不健康的旧信号：

```text
profile=(string)high-4:4:4
profile-level-id=(string)f40020
Capture pipeline exited with error: exit status 1
WebRTC RTP stats: packets=0 bytes=0
```

## 远端部署步骤

### 1. 本地测试

```bash
cd /Users/shenlan/workspaces/ai-workspace-lab/xworkmate-bridge
go test ./internal/desktop ./internal/acp
```

如果 APP 侧同步有诊断改动，同时执行：

```bash
cd /Users/shenlan/workspaces/ai-workspace-lab/xworkmate-app
flutter test test/features/desktop/desktop_client_test.dart
flutter analyze lib/features/desktop/desktop_client.dart lib/features/desktop/desktop_view.dart test/features/desktop/desktop_client_test.dart
```

### 2. 构建 Linux binary

远端服务运行在 Linux x86_64。不要把 macOS 本机构建物直接部署到远端，否则会出现 `Exec format error`。

推荐命令：

```bash
cd /Users/shenlan/workspaces/ai-workspace-lab/xworkmate-bridge
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 OUTPUT_PATH=build/bin/xworkmate-go-core-linux-amd64 make build
file build/bin/xworkmate-go-core-linux-amd64
```

### 3. 部署远端

```bash
cd /Users/shenlan/workspaces/ai-workspace-lab/xworkmate-bridge
scripts/github-actions/deploy-native-binary.sh xworkmate-bridge.svc.plus build/bin/xworkmate-go-core-linux-amd64 <short-commit>
```

脚本会：

- 恢复 `BRIDGE_AUTH_TOKEN`
- 保留/恢复 `BRIDGE_REVIEW_AUTH_TOKEN`
- 上传 binary 到远端
- 安装到 `/home/ubuntu/.local/bin/xworkmate-go-core`
- 重启 `systemctl --user` 的 `xworkmate-bridge.service`
- 校验部署后的 commit

### 4. 确认远端版本

```bash
ssh ubuntu@xworkmate-bridge.svc.plus '
  systemctl --user --no-pager --full status xworkmate-bridge
  /home/ubuntu/.local/bin/xworkmate-go-core version
'
```

期望看到：

- `Active: active (running)`
- `commit` 等于刚部署的提交

GitHub Actions 发布成功只能说明 CI/CD 完成。现场仍要确认远端二进制和 HTTPS API：

```bash
gh run view 27095512721 --repo ai-workspace-lab/xworkmate-bridge
ssh ubuntu@xworkmate-bridge.svc.plus '/home/ubuntu/.local/bin/xworkmate-go-core version'
```

如果 APP 关于页仍显示 `unauthorized`，继续执行“前置检查：先排除账号同步 / 鉴权问题”，不要进入 RTP 判定。

### 5. 确认 Caddy / service 没有漂移

当前生产形态以 user service 为准：

```bash
ssh ubuntu@xworkmate-bridge.svc.plus '
  echo "system=$(systemctl is-active xworkmate-bridge.service 2>/dev/null || true)"
  echo "user=$(systemctl --user is-active xworkmate-bridge.service)"
'
```

期望：

- `system=inactive`
- `user=active`

如果 system service 处于 `activating` / `failed` / 自动重启，可能与 user service 抢占 `127.0.0.1:8787`，需要先清理 system service 冲突。

## 白屏排查步骤

### A. 先看服务与版本

```bash
ssh ubuntu@xworkmate-bridge.svc.plus '
  systemctl --user --no-pager --full status xworkmate-bridge
  ps -fp $(systemctl --user show -p MainPID --value xworkmate-bridge.service)
'
```

同时确认 APP 侧账号同步已成功。如果 APP 显示 `Bridge token expired or rejected`，先重新同步账号 token。此时日志里通常不会出现新的远程桌面 session，说明请求没有进入 WebRTC 层。

### B. 跟日志

```bash
ssh ubuntu@xworkmate-bridge.svc.plus '
  journalctl --user -f -u xworkmate-bridge
'
```

重点观察：

- `Starting Remote Desktop session`
- `Starting capture pipeline`
- `Capture pipeline exited with error`
- `profile=(string)baseline`
- `profile-level-id=(string)42c01f`
- `WebRTC RTP stats`
- `WebRTC RTP final stats`

### C. 连接 APP，重复执行

建议至少跑三轮：

1. 首次连接
2. 断开再连接
3. 快速重连

如果问题只在重连出现，要重点看：

- 旧 session 是否被 `Stopping Remote Desktop session` 正常清理
- 旧 pipeline 是否退出
- 新 session 是否出现新的 RTP stats 增长

### D. 判定编码是否兼容

以下信号说明编码兼容性已进入 WebRTC 友好区间：

- `format=(string)I420`
- `profile=(string)baseline`
- `profile-level-id=(string)42c01f`
- `packetization-mode=(string)1`
- `config-interval=1`

### E. 判定 RTP 是否真的在发

看 `WebRTC RTP stats`：

- `packetDelta > 0`
- `byteDelta > 0`
- `writeErrors=0`

如果这些值连续多个周期都不增长：

- 优先查 GStreamer / FFmpeg capture 是否退出
- 再查 display / X11 / encoder 参数

## APP 侧 stats 判读

APP 在等待首帧时会定期打印 inbound video stats 摘要。

关键字段：

- `packetsReceived`
- `bytesReceived`
- `framesDecoded`
- `framesDropped`
- `keyFramesDecoded`
- `jitter`
- `jitterBufferDelay`

判读：

- `packetsReceived == 0`
  - Bridge 在发，但对端没收到，优先查 ICE / candidate / 网络。
- `packetsReceived > 0 && framesDecoded == 0`
  - 收到 RTP 但解码不了，优先查 H.264 profile / SPS/PPS / browser 兼容。
- `framesDecoded > 0`
  - 编码与网络基本通，继续查 renderer / stale stream / attach。

## 现场验证模板

### 一次健康验证应满足

1. Bridge answer / RTP caps 中 H264 协商属于 baseline family：

```text
profile-level-id=42c01f
packetization-mode=1
```

说明：`profile-level-id` 以 `42` 开头是 baseline family 的关键特征。现场日志中 `rtph264pay` 常见值为 `42c01f`；不应再出现旧的 `f40020`。

2. GStreamer caps 中包含：

```text
format=(string)I420
profile=(string)baseline
width=(int)1280
height=(int)720
```

3. RTP 统计连续增长：

```text
WebRTC RTP stats: packets=1471 ...
WebRTC RTP stats: packets=2977 ...
WebRTC RTP stats: packets=4496 ...
```

4. 结束会话时看到 final stats：

```text
WebRTC RTP final stats: packets=9470 bytes=7962965 writeErrors=0
```

## 常见问题

### 1. `cannot execute binary file: Exec format error`

原因：

- 把 macOS binary 部署到了 Linux 远端。

修复：

- 用 `CGO_ENABLED=0 GOOS=linux GOARCH=amd64` 重新构建。

### 2. `Capture pipeline exited with error: exit status 1`

高概率原因：

- 4:2:0 输入尺寸为奇数
- profile / format 约束与实际 caps 冲突

修复：

- 强制 `videoscale`
- 输出归一化到偶数 `width/height`

### 3. 旧 service inactive，但 8787 仍被占用

说明：

- 需要区分 system service 和 user service
- 以 `systemctl --user status xworkmate-bridge` 为准

### 4. `packetsReceived` 增长但仍白屏

优先看：

- `framesDecoded`
- `videoWidth` / `videoHeight`
- 前端 renderer 是否拿到首帧

### 5. APP 显示 `Bridge token expired or rejected`

说明：

- 这是账号同步 / token 前置问题，不是 H.264 编码或 RTP 发送问题。
- Bridge 可能已经部署到最新 commit，且带 token 的 `/api/ping` 正常。
- APP 不会成功发起 `xworkmate.desktop.offer`，所以 Bridge 日志中不会出现新的 desktop session。
- 对 `review@svc.plus`，还要确认 Caddy 公网入口接受 `BRIDGE_REVIEW_AUTH_TOKEN`。只测 origin `127.0.0.1:8787` 不够。

修复：

- 在 APP 设置页点击 `重新同步`。
- 刷新版本信息，确认 Bridge runtime `Status` 不再是 `unauthorized`。
- 确认公网 `/api/ping` 对主 token 和 review token 都返回 `200`，无 token 返回 `401`。
- 再重新连接远程桌面并观察 RTP / stats。

## 回滚

1. 用上一个稳定 commit 重新构建 Linux binary。
2. 重新执行部署脚本。
3. 重启 user service。

示例：

```bash
cd /Users/shenlan/workspaces/ai-workspace-lab/xworkmate-bridge
git checkout <stable-commit>
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 OUTPUT_PATH=build/bin/xworkmate-go-core-linux-amd64 make build
scripts/github-actions/deploy-native-binary.sh xworkmate-bridge.svc.plus build/bin/xworkmate-go-core-linux-amd64 <stable-commit>
```

## 文档维护要求

- 新增 WebRTC 白屏修复时，优先补本 runbook，不要只留在聊天记录里。
- 新增关键日志时，必须说明默认是否开启、是否限频、如何关闭。
- 如果 H.264 参数再调整，务必同步更新：
  - 期望 profile
  - 期望 pixel format
  - RTP 判读标准
  - 远端部署命令
