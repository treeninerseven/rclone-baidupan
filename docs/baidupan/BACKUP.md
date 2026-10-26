# 百度网盘备份与恢复

当前实验后端已支持上传、同名覆盖、递归创建目录、空文件、空目录、中文及特殊文件名，
并保留文件修改时间。上传、恢复和增量更新已在真实账号的应用目录中验证。

需要加密文件内容、文件名和目录名时，使用已实测通过的 crypt 组合，见 [ENCRYPTION.md](ENCRYPTION.md)。

## 备份

从项目根目录执行。将第一处示例路径替换为需要备份的本地目录；远端目录也可以自行命名：

```sh
cd rclone-baidupan
./rclone --config .local/rclone.conf copy "/本地/需要备份的目录" \
  "baidu:apps/rcloneBaidu/backups/my-files" \
  --create-empty-src-dirs --progress
```

`copy` 会上传新文件、更新变化的文件，并跳过没有变化的文件；
远端存在而本地没有的文件会保留。同名覆盖会替换旧内容，不会自动保存带日期的历史版本。
当前已验证的写入位置是 `/apps/rcloneBaidu/`；不要把可读取全盘理解为具有全盘写入权限。

## 校验与恢复

对照本地原文件，下载远端内容并检查一致性：

```sh
./rclone --config .local/rclone.conf check "/本地/需要备份的目录" \
  "baidu:apps/rcloneBaidu/backups/my-files" --download
```

这会读取文件内容并消耗下载流量。后端暂不声明百度返回的 MD5 是任意已有文件的内容哈希，
因此建议使用 `--download` 做内容校验。

恢复到一个新的本地目录：

```sh
./rclone --config .local/rclone.conf copy \
  "baidu:apps/rcloneBaidu/backups/my-files" "/本地/新建的恢复目录" \
  --create-empty-src-dirs --progress
```

## 临时空间和大文件

百度预上传需要先提交全部分片 MD5。后端会先把每个上传文件暂存到本地临时文件，
再上传；成功、失败或取消后均清理临时文件。临时磁盘需要容纳正在上传的所有文件。
可通过 `--temp-dir` 选择临时磁盘，通过 `--transfers 1` 降低同时暂存的文件数量。

默认分片为 4 MiB，当前每个文件最多 1024 片，即默认单文件上限为 4 GiB。
有相应会员权限时，可使用 `--baidupan-chunk-size 16M` 或 `32M`。
百度账号自身的文件大小和分片权限仍然适用；大于本轮 8 MiB 测试文件的场景尚未充分验证。

目前没有跨进程上传续传功能。失败后重新运行备份会跳过已完成且未变化的文件，
未完成的文件会重新开始。文件修改时间精度为一秒；已上传文件不能单独修改时间，
需要重新上传。源文件在备份期间持续变化时，不提供文件系统快照一致性保证。

## 已验证的测试备份

测试文件保留在：

```text
baidu:apps/rcloneBaidu/rclone-backup-test-20261026-121331
```

包含 5 个生成的测试文件，其中一个超过 8 MiB，以及一个空目录。
上传后恢复得到的全部文件 SHA-256 与本地源文件一致。
也验证了同大小不同内容文件的更新、未变化文件跳过、空文件和特殊文件名恢复。
详细记录见 [TESTING.md](TESTING.md)。
