# 图片 JSON 响应统一格式

适用于 OpenAI 兼容 Images API 的非流式生成和编辑响应，包括本站 OpenAI 类型的上游渠道。SSE 流式事件保持原协议，不转换为这个 JSON 结构。

成功响应的 `data` 必须是非空对象数组。未指定 `response_format` 或指定 `"b64_json"` 时，每张图片都返回经过验证的非空 `b64_json`，保持原有行为。合法的普通 Base64 原样保留；带 `data:image/...;base64,` 前缀的值移除前缀，图片字节保留，格式依据真实文件识别。上游只给 URL 时下载完整图片并编码为 Base64，原 URL 保留兼容。保留 usage 及未知上游扩展字段；固定补齐缺失的顶层 `created`、`size`、`output_format`、`quality`、`background`、`usage`，无法确定的元数据为 JSON `null`。

指定 `"response_format": "url"` 时，每张图片改为返回本站 `data[i].url`，不返回 `b64_json`。图片由本站临时保存，内容不裁剪、不拉伸、不转码，真实尺寸和 usage 保留。链接有效期为生成链接后的 24 小时，每张图片的 `expires_at` 为 Unix 秒时间戳；到期返回 HTTP 410 并由后台清理文件。链接可直接通过 GET/HEAD 访问，不需要登录或 API Key，持有该随机链接的人可以访问图片，请及时下载并按需保密。此选择同样适用于 JSON/multipart 编辑和后台任务；仅支持非流式请求，`stream: true` 会在调用上游前拒绝。

```json
{"model":"gpt-image-2.5","prompt":"A ceramic cup on a white background","size":"1024x1024","n":1,"response_format":"url"}
```

```json
{
  "created": 1788777442,
  "data": [{
    "url": "https://example.com/image.png",
    "b64_json": "iVBORw0KGgo...完整图片的Base64编码...",
    "revised_prompt": null,
    "width": 1199,
    "height": 1312,
    "size": "1199x1312",
    "output_format": "png"
  }],
  "size": "1199x1312",
  "output_format": "png",
  "quality": null,
  "background": null,
  "usage": null
}
```

`data[i].width/height/size/output_format` 从同一份图片数据识别，不使用请求中的期望尺寸代替实际尺寸。支持 PNG、JPEG、GIF、WebP。无效 Base64、非图片、不可解码的像素内容和空图片列表返回交付错误，不能作为成功 JSON 返回。GIF 的标准解码器验证首帧，未对全部动画帧进行验证。URL-only 图片必须先成功下载并验证，才返回成功 JSON。

无论上游是否提供 `size`，顶层和每张图片的 `size` 都根据图片文件的实际宽高重新生成，覆盖上游的尺寸声明。单张图片或全部图片实测尺寸相同时，顶层返回该尺寸；多图尺寸不同或有图片无法读取时，顶层为 `null`，每张图片分别返回自己的实测尺寸。`output_format` 的顶层值也根据实际文件格式生成；无法识别或多图格式不一致时为 null，不保留与文件不符的上游声明。

URL 下载使用现有 SSRF 保护客户端和受控重定向，不传递渠道 Key、用户认证头、Cookie 或重定向 Referer。完整下载不发送 Range；只接受 HTTP 200，不把部分内容编码成完整图片。原生 Base64 和下载图片均限制单张文件最多 32 MiB、响应累计 Base64 内容最多 48 MiB；下载共享 30 秒超时，最多 128 张图片。单边最多 16384 像素，总像素最多 16777216，像素解码限制两路并发。尺寸识别复用生成的 Base64，不进行第二次网络下载。

URL 下载超时、失败、被访问策略阻止、图片无效或超限时，普通 Images API 返回 HTTP 424、错误码 `image_delivery_failed` 和 `x-should-retry: false`。服务端跳过自动生图重试，并沿用现有流程退回预扣额度。成功响应的计费 usage 仍来自原始上游响应。

网页批次的私有响应暂存通道同样验证原生 Base64，保留 URL-only 图片原有下载恢复机制：下载失败可从暂存响应恢复，不会重新调用生图上游。该通道由服务端身份上下文标记，不能通过客户端请求字段启用；不是公开 Images API 响应。

调用方保存图片时应逐张使用 `data[i].size/output_format`。多图尺寸或格式不同，顶层字段仍为 `null`。返回图片数量与有效请求数量不符时增加 `requested_n`、`returned_n` 和 `output_warnings` 中的 `image_count_mismatch`，保留所有返回图片、真实像素和现有计费规则；该提示不证明某张图是参考图回显。

格式化后重新计算 Content-Length，删除原响应体校验头。非法 JSON、缺少有效 data 数组或非图片错误响应不会被伪装成成功图片响应。

## 发布记录

2026-09-07 已部署至 `hardy777.top`，版本 `hardy-image-json-20260907-194257`，镜像 `hardy777/new-api:image-json-20260907-194257`。目标为 `140.245.89.202` / `a1-ubuntu-free` 的 ARM64 `new-api` 应用容器。本地功能提交 `d330e43554e94a10538840644d02e84f22d39945`；服务器源码提交 `2fc75de3a13d16a514ef44421ffa603f540db981`。

在禁止外网访问的隔离环境恢复生产 PostgreSQL，使用最终发布镜像和模拟图片上游，验证 Base64、URL、多图不同尺寸、URL 过期和已有元数据五种响应。基础字段齐全，实际尺寸来自 PNG 文件头，图片/扩展字段和 usage 保留，Content-Length 正确、旧 ETag 移除，原有 2 分单价扣费不变；未调用收费上游。

上线后公网状态返回新版本，容器 healthy、重启次数 0，生产运行镜像与隔离验收镜像一致。隔离容器和网络已清理。备份及切换前数据库恢复点：`/opt/new-api-releases/20260907-194257/backup`；回滚镜像：`hardy777/new-api:before-image-json-20260907-194257`。

后续尺寸规则修正已发布为 `hardy-image-json-20260907-210820`（镜像 `hardy777/new-api:image-json-20260907-210820`）：顶层 `size` 也强制使用实测宽高，不再保留上游声明。新增回归测试及隔离验收覆盖上游错误尺寸被覆盖、多图尺寸不同，以及读取失败时不沿用未经验证的尺寸。Chrome 可视对照确认上游声明 `1024x1024`、图片实测 `120x80` 时返回 `120x80`。

该修正本地提交 `e4c88e730018f71e21f10fcb3b3df714d2eec209`，服务器源码提交 `9890475590b3606df315b3e88db03f5a4ad91a99`。备份：`/opt/new-api-releases/20260907-210820/backup`；回滚镜像：`hardy777/new-api:before-image-json-20260907-210820`。

2026-09-08 已部署统一 Base64 响应，版本 `hardy-image-b64-20260908-104243`，镜像 `hardy777/new-api:image-b64-20260908-104243`。本地功能提交 `c3612103a58abe874247614fb0668e54c713303b`；服务器源码提交 `8541770efbc68f186de1214e656de32e52a42fb3`。

相关 service、OpenAI relay 和 controller 回归测试通过。最终镜像在隔离生产数据库副本中通过原生 Base64、URL 转 Base64、混合图片、过期 URL、上游元数据五种验收；验证 usage 与原有单价不变，下载失败返回 424、禁止自动生图重试并退回预扣额度。未调用收费上游。Chrome 可视验收确认 URL 图片使用返回的 Base64 成功显示，实测尺寸为 120x80；失败场景返回 image_delivery_failed。

公网与本机状态均确认新版本，生产容器 healthy、重启次数 0，运行镜像与隔离验收镜像一致，隔离容器和网络已清理。备份：`/opt/new-api-releases/20260908-104243/backup`；回滚镜像：`hardy777/new-api:before-image-b64-20260908-104243`。服务器应用源码补丁时仅调整文档上下文，发布二进制未改变。

2026-10-12 已部署可选择 URL 返回，版本 `hardy-image-url-20261012`，运行镜像 `sha256:68a03935a5d05c80e1a237dd483b26904e2207153860e2421ba648abaab75070`。请求 `response_format: "url"` 时返回本站 24 小时临时图片链接并省略 Base64，默认格式保持原样。模拟上游验证原图、JSON/multipart 参数适配、无鉴权 GET/HEAD、到期、清理、多图失败回滚及单次计费；未调用收费上游。7 个生产代码文件按 OCR 委托流程审查完毕。通过受限 `hardy-app-deploy` 切换，切换前 API 后台任务和绘图任务均为 0；公网确认新版本、失效链接 410、非法链接 404，前端构建版本保持不变。证据在 `artifacts/image-url-format-20261012/`。
