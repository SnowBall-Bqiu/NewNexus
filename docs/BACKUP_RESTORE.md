# 数据备份与恢复

备份必须同时保留数据库和 `NEWNEXUS_MASTER_KEY`。主密钥应独立存入密码管理器或密钥系统；丢失后无法恢复连接、TOTP 和通知凭据。Agent 的 `/data` 包含凭据和未确认结果，应持久化且仅限运行用户访问。

## SQLite

使用 Python 3.11+ 的 SQLite Online Backup API，可以对运行中的 WAL 数据库做一致性备份：

```sh
python3 scripts/sqlite-backup.py backup /path/to/data/newnexus.db /backup/newnexus-20261106.db
python3 scripts/sqlite-backup.py verify /backup/newnexus-20261106.db
```

脚本拒绝覆盖已有文件，备份后执行完整性检查并生成 SHA256。不要直接复制仍在写入的 `.db` 文件；只复制主文件会丢失 WAL 中的事务。容器卷可通过受控的临时维护容器挂载执行该脚本。

恢复时先停止唯一控制面实例，保留原目录作为回退，选择一个新的空数据目录：

```sh
python3 scripts/sqlite-backup.py restore /backup/newnexus-20261106.db /new-data/newnexus.db --stopped
```

恢复同一主密钥、更新数据卷挂载，再启动服务。检查健康状态、管理员登录、解密连接、节点重连、作业和最近发布；重新同步厂商 observed 状态，审查漂移后再恢复自动发布。备份中的已消费外部作业可能早于厂商当前状态，必须先核对。

## MySQL 8.4

使用与 MySQL 8.4 兼容的 `mysqldump` / `mysql` 客户端。生产环境另外配置 binlog 的保留、归档与 PITR；逻辑全量备份不替代按恢复目标设计的增量恢复。以下为 POSIX shell 示例；为每次备份选择新文件名，文件及主密钥分别保管。

将专用备份账户写入权限 `0600` 的 `/etc/newnexus/backup.cnf`，避免将密码放在进程参数和命令历史中。例如跨主机连接使用：

```ini
[client]
host=db.example.com
port=3306
user=newnexus_backup
password=REPLACE_LOCALLY
ssl-mode=VERIFY_IDENTITY
ssl-ca=/etc/newnexus/db-ca.pem
```

备份账户由数据库管理员单独配置，具有目标库的读取、视图、触发器、事件及存储程序定义读取权限；不要因备份给控制面应用账户授予全局管理权限。`--no-tablespaces` 避免导出 tablespace 定义所需的额外权限。选项文件须作为客户端的第一个参数。

`--single-transaction` 为 InnoDB 表建立一致性快照。备份期间不能执行表结构迁移或分区 DDL；NewNexus 会维护指标分区，最直接的维护窗口做法是先暂停自动发布并停止全部控制面副本，Agent 将未确认结果留在本地队列。不要在备份期间启动其他版本的控制面。

```sh
set -eu
umask 077
backup_file=/backup/newnexus-20261107.sql
test ! -e "$backup_file"
mysqldump --defaults-extra-file=/etc/newnexus/backup.cnf \
  --single-transaction --quick --hex-blob --default-character-set=utf8mb4 \
  --routines --events --triggers --no-tablespaces --set-gtid-purged=OFF \
  --result-file="$backup_file" newnexus
sha256sum "$backup_file" > "$backup_file.sha256"
```

仅在命令成功退出后保存并转移备份。失败时把残缺文件标为失败或移至隔离位置，不能因文件存在就视为备份成功。上例没有 `--databases`，SQL 不会把恢复目标固定回原库。备份包含管理员、加密凭据、会话和审计数据，应加密存储并限制访问。

恢复前停止全部 API、调度与 gRPC 副本，保留原库作为回退，由数据库管理员建立新的空库并授权恢复账户。不要覆盖仍提供服务的生产库：

```sql
CREATE DATABASE newnexus_restore CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin;
```

为目标服务创建单独的 `/etc/newnexus/restore.cnf`（`0600`），然后检查备份校验值并恢复：

```sh
sha256sum --check /backup/newnexus-20261107.sql.sha256
mysql --defaults-extra-file=/etc/newnexus/restore.cnf \
  --default-character-set=utf8mb4 --database=newnexus_restore \
  < /backup/newnexus-20261107.sql
mysql --defaults-extra-file=/etc/newnexus/restore.cnf \
  --database=newnexus_restore \
  --execute="CHECK TABLE schema_migrations,resources,result_ids,metrics,job_leases,idempotency,singleton_locks,metrics_5m,metrics_1h;"
```

任一命令失败都停止切换。逻辑恢复包含 DDL，不能通过一个大事务保证整库回滚；失败后保留日志，在另一个新空库重新演练。用目标 URL 执行 `newnexus-server --migrate`，恢复相同 `NEWNEXUS_MASTER_KEY`，核对表/资源/任务/指标数量、最近发布时间和抽样解密。重新同步厂商 observed 状态、审查漂移，确认节点重连和作业租约后再恢复自动发布。外部 DNS 可能已晚于备份状态，不能仅按旧作业状态重复发布。

这些命令是恢复流程；实际通过的演练及数据库版本记录在 [验收清单](ACCEPTANCE.md)，尚未执行的流程不计为验收通过。MySQL 参数及快照/DDL 限制参见官方 [mysqldump 文档](https://dev.mysql.com/doc/refman/8.4/en/mysqldump.html)；binlog 恢复参见 [备份恢复手册](https://dev.mysql.com/doc/refman/8.4/en/backup-and-recovery.html)。

## MySQL 结构升级与指标分区

首次升级到包含 v2 及后续迁移的版本时，服务会为 JSON 资源字段添加生成列，并为任务、会话、结果收据、汇总指标和候选筛选补齐索引。MySQL 的这些 `ALTER TABLE` 会隐式提交，整批迁移不能作为一个事务整体回滚。迁移使用固定连接上的 `GET_LOCK` 串行执行，防止多个控制面同时修改结构；该锁不提供整批 DDL 回滚能力。

升级应使用维护窗口：暂停自动发布，停止所有旧版控制面副本，按上文完成并校验备份，再仅运行一次新版 `newnexus-server --migrate`。迁移成功后启动新版副本，核对数据库、节点重连、作业和最近发布状态。迁移失败时保留错误日志及原备份，先处理失败原因；需要回退数据时恢复到新空库，不直接覆盖原库或同时启动新旧版本。

指标表通常只预建 UTC 当天、后一天和后两天的分区；合法晚到数据需要的历史日期分区由服务按需建立。范围分区中的第一个分区具有隐式 `MINVALUE` 下界，分区名不能代表严格的日期下界。不要依据分区名称手工删除历史分区，应使用系统的保留清理流程并核对实际分区边界。

恢复旧版本 dump 后，先保持全部控制面停止，用新版对恢复的新库执行 `--migrate`，补齐索引、生成列及未来日期分区，再开始服务验收。恢复流程不需要改写备份中的指标分区定义。

## 从 PostgreSQL 切换

更换 `NEWNEXUS_DATABASE_URL` 只切换连接，不会转换既有 PostgreSQL 数据。`--migrate` 是目标库内的结构升级，也不会读取或改写原 PostgreSQL 库。保留旧库及主密钥，先在隔离环境设计跨引擎数据转换，停写后核对所有资源、任务、幂等收据、指标、发布快照和审计数据，再执行切换。没有跨引擎转换工具及对应验收时，不应直接用一个空 MySQL 库代替已有生产库。历史 PostgreSQL 备份可继续使用原版本的 `pg_dump` / `pg_restore` 维护，不能直接导入 MySQL。
