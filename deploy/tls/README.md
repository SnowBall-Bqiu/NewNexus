# gRPC TLS 证书目录

Docker Compose 默认从本目录挂载 `grpc.crt` 和 `grpc.key`。证书的 SAN 必须覆盖 `NEWNEXUS_AGENT_PUBLIC_SERVER` 使用的主机名，私钥文件应限制为管理员可读。

生产环境建议使用受信任 CA 签发的证书。主控启动后会从叶证书自动计算 SHA-256 指纹，并写入控制台生成的 Agent 一键安装命令；也可以用 `NEWNEXUS_AGENT_TLS_SHA256` 显式覆盖。

仅在非容器部署、gRPC 明文监听明确绑定到回环或私网 IP、且 TLS 由同机可信反向代理终止时，才同时清空两个证书文件变量并设置 `NEWNEXUS_GRPC_ALLOW_H2C=true`。
