# 备份与恢复

必须同时备份数据库和 `NEWNEXUS_MASTER_KEY`。只恢复数据库而丢失主密钥会导致厂商、SMTP 与 Webhook 凭据无法解密。

## SQLite

```bash
docker compose -f docker-compose.yaml --profile sqlite stop control-plane-sqlite
docker run --rm -v newnexus_newnexus-data:/data -v "$PWD/backups:/backup" alpine \
  cp /data/newnexus.db /backup/newnexus-$(date +%F-%H%M).db
docker compose -f docker-compose.yaml --profile sqlite start control-plane-sqlite
```

恢复时停止控制端，将选定备份复制为 `/data/newnexus.db`，确认所有者可读写，再使用原主密钥启动。SQLite 只允许一个控制面实例。

## PostgreSQL

```bash
docker compose -f docker-compose.yaml exec -T postgres \
  pg_dump -U newnexus -d newnexus -Fc > backups/newnexus-$(date +%F-%H%M).dump
```

恢复到空数据库后运行 `pg_restore --clean --if-exists`，再启动单个控制端完成迁移。检查 `/readyz`、迁移版本、管理员登录、Agent 在线、连接验证和一条只读 DNS 预览。

每季度至少执行一次隔离恢复演练，记录备份时间、恢复耗时、行数抽查和主密钥校验结果。
