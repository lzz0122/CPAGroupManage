# CPA 分组插件迁移与升级手册

本文档用于把旧 CPA 单体分组定制迁移到独立插件，并在后续 CPA 官方更新时把合并范围控制在最小宿主补丁内。

## 1. 组件边界

升级时分别处理三个仓库：

- `cpa-backend-plugin-host`：官方 CPA 加通用、最小插件宿主接口；
- `cpa-plugin-lzz-groups`：全部分组策略、管理 UI 和稳定管理 API；
- `new-api`：CPA 分组插件适配器。

不要再把分组字段、Key 策略、分组页面或 NewAPI 专用接口合并回 CPA 主仓库。

## 2. 首次迁移前备份

至少保存以下内容：

- 当前 CPA 二进制、镜像标签和完整 `config.yaml`；
- `auth-dir` 中的 OAuth JSON；
- 当前插件目录；
- NewAPI 镜像、数据库备份和环境变量；
- 旧单体源码分支或归档分支；
- 当前反向代理上游与容器网络信息。

备份中包含密钥，必须使用受限权限保存，不能提交到 Git。

## 3. 旧配置迁移

按以下规则迁移：

1. 旧 `api-key-policies` 中的每个 Key 转为插件 `api-keys` 项。
2. OAuth/auth-file JSON 中原有的 `group` 或 `groups` 可以由插件首次读取，但应在管理页确认后保存进插件配置。
3. `gemini-api-key`、`claude-api-key`、`codex-api-key`、`xai-api-key`、`openai-compatibility` 等配置型凭据的旧 `groups` 字段不会由官方 CPA 解析，必须在插件管理页按 `auth_index` 重新分组。
4. 先把所有仍需使用的下游 Key 写入插件，再启用插件；插件认证是独占模式。
5. 验证完成前不要删除旧字段和旧单体镜像。

## 4. 候选构建

CPA 宿主：

```bash
gofmt -w internal/pluginhost/auth_callbacks.go \
  internal/pluginhost/host_callbacks.go \
  internal/pluginhost/scheduler.go \
  sdk/cliproxy/auth/conductor_selection.go \
  sdk/pluginabi/types.go \
  sdk/pluginapi/types.go
go test ./internal/pluginhost ./sdk/cliproxy/auth ./sdk/pluginabi ./sdk/pluginapi
go build -o test-output ./cmd/server
rm test-output
```

插件：

```bash
gofmt -w *.go
go test ./...
go build -o /tmp/cpa-plugin-lzz-groups-check .
rm /tmp/cpa-plugin-lzz-groups-check
go build -buildmode=c-shared -o bin/lzz-groups.so .
```

NewAPI：

```bash
go test ./service ./controller
```

构建产物不要提交到 Git。生产发布仍使用候选镜像、原子切换和可立即恢复的旧镜像。

## 5. 上线前验收清单

在隔离端口或候选容器完成：

- 插件显示为 registered/enabled；
- `分组管理` 菜单和资源页可打开；
- OAuth/auth-file 与配置型 API Key 都出现在插件 state；
- state 中不出现完整上游 API Key；
- 配置保存后 CPA 热重载成功；
- 受限 Key 只能看到自己分组的插件模型列表；
- 错误下游 Key 返回 401；
- 高优先级错误组、低优先级正确组时选择低优先级账号；
- round-robin、fill-first、weighted 和 session affinity 行为仍由 CPA configured selector 决定；
- 非空流量请求能够得到正确的 `X-CPA-TRACE-ID` auth index；
- NewAPI 手工获取模型、自动同步模型、quota health 与拼车额度读取均正常。

## 6. NewAPI 切换

在 NewAPI 容器中配置：

```text
CPA_MANAGEMENT_KEY=<CPA_MANAGEMENT_KEY>
```

该值应通过部署环境或 secret 注入，不写入镜像和 Git。NewAPI 与 CPA 在同一 Docker 网络时继续使用内部地址，例如 `http://cli-proxy-api:8317`。

设置后：

- 渠道模型列表改走插件 `/models`；
- CPA quota health 改走插件 `/quota-health`；
- 拼车额度中的账号分组以插件 `/state` 为准。

未设置环境变量时，NewAPI 保留旧接口回退，仅用于迁移期。

## 7. 后续 CPA 官方升级

推荐流程：

1. 从新的官方 tag 或 `origin/main` 创建升级分支。
2. 只移植 CPA 最小宿主提交，不移植旧单体分组提交。
3. 重点解决六个宿主文件的冲突；若官方 API 变化，再调整通用契约文档。
4. 运行宿主测试与构建。
5. 用现有插件二进制做兼容握手；为保证可复现，正式发布前仍建议从插件源码重新构建。
6. 重跑第 5 节的隔离验收。
7. 保留旧镜像和配置快照后再原子切换。

如果官方未来原生提供等价的 candidate filter 和 runtime inspection，优先把本地宿主补丁收敛为官方接口适配，不要在插件中复制调度器。

## 8. 回滚

插件版本回滚：

1. 恢复上一版 `.so`；
2. 重启候选 CPA；
3. 确认插件握手和 state；
4. 再切回生产上游。

回滚到旧单体 CPA：

1. 恢复旧 CPA 镜像与对应配置；
2. 禁用 `lzz-groups` 插件，避免两套独占认证同时存在；
3. 移除或关闭 NewAPI 的 `CPA_MANAGEMENT_KEY`，使其回到旧接口；
4. 验证模型、quota health、分组路由和拼车额度；
5. 再完成反向代理切换。

任何回滚都必须保持单一活动写入节点，避免两套 CPA 同时修改同一配置或 auth 目录。

## 9. Git 维护

- 每个升级功能使用独立分支；
- 测试通过后才合并主线；
- commit 使用 `type(scope): 描述`；
- 不把二进制、测试临时文件、部署环境文件或密钥提交到 Git；
- commit 前运行 `git diff --check`、检查未跟踪文件，并扫描疑似密钥；
- 未经明确授权不 push。
