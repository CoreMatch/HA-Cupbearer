<div align="center">
  <h1>Armorial Cupbearer</h1>
  <p><strong>从暂存到健康检查，发布过程可控。</strong></p>
  <p><a href="./README.md">English</a></p>
</div>

Armorial Cupbearer 通过以裸金属为先的控制路径交付和部署 Armorial 服务。它整合产物暂存、软链激活、健康与冒烟检查、回滚，以及由它监督运行的 MaintenanceView 运维界面。

## 为什么选择 Armorial Cupbearer

- **校验和暂存。** 基于校验和的暂存路径在激活前准备产物文件。
- **激活并验证。** 软链切换后执行健康检查和可选冒烟检查，裸金属变更失败时可回滚。
- **为高风险操作设门。** 分级预检与凭据引用暴露阻断项，同时避免把秘密值写入清单。
- **初始化可续跑。** 可恢复的初始化流程和受监督的 MaintenanceView 减少重复操作。
- **容器按摘要回滚。** 托管容器回滚锁定记录的镜像摘要，而非可移动的标签。

## 进一步了解

- [Docker Hub 上的 Armorial Cupbearer](https://hub.docker.com/r/limeight/ha-cupbearer) — 已发布容器镜像
- [GitHub 上的 Armorial Cupbearer](https://github.com/CoreMatch/HA-Cupbearer) — 规范源码仓库
- [Armorial Cupbearer API 契约](https://github.com/CoreMatch/HA-Contract/blob/main/docs/api/openapi/ha-cupbearer.yaml) — 版本化交付接口
- [MaintenanceView](https://github.com/CoreMatch/HA-Services-MaintenanceView) — 运维控制台

## 边界

清单声明的产物字节不进行哈希校验，并非所有交付路径都有签名，容器触发不会自动回滚，且当前生产使用直接原子切换而非 Cupbearer 编排。

## 许可证

[AGPL-3.0](LICENSE)
