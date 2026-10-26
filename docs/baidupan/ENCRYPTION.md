# 百度网盘加密备份

使用 rclone 内置的 `crypt` 包装现有 `baidu` 后端。
数据在本地加密后才交给百度网盘后端；通过 crypt 恢复时自动解密。
选择标准名称加密后，包装目录内部的文件名和目录名也会加密。

真实账号测试已通过：4 个文件解密恢复后的 SHA-256 全部匹配，空文件、空目录、
跨加密块的分段读取均正常；错误密码和被修改的密文都会被拒绝。
测试包括超过 4 MiB 的多分片文件，但不等同于完整的安全审计或多 GiB 压力验证。

## 配置正式加密备份

从项目根目录执行：

```sh
cd rclone-baidupan
./rclone --config .local/rclone.conf config
```

新建 remote，填写：

| 配置项 | 填写内容 |
| --- | --- |
| 名称 | `baiducrypt` |
| 存储类型 | `crypt` |
| remote | `baidu:apps/rcloneBaidu/encrypted-backups` |
| filename_encryption | `standard` |
| directory_name_encryption | `true` |
| password | 自己设置的强密码，与百度登录密码、SecretKey 分开 |
| password2 | 可选的另一独立口令，作为自定义盐；设置后也必须妥善备份 |

保存后退出配置。使用专门的加密目录，不要把已有明文备份目录直接混作密文目录。
已有明文文件不会自动转换，需要通过 `baiducrypt:` 重新上传。

## 备份、恢复和内容检查

备份：

```sh
./rclone --config .local/rclone.conf copy "/本地/需要备份的目录" baiducrypt: \
  --create-empty-src-dirs --progress
```

恢复到新的本地目录：

```sh
./rclone --config .local/rclone.conf copy baiducrypt: "/本地/新建的恢复目录" \
  --create-empty-src-dirs --progress
```

下载解密后检查内容：

```sh
./rclone --config .local/rclone.conf check "/本地/需要备份的目录" baiducrypt: --download
```

通过 `baiducrypt:` 访问时看到正常文件名和明文内容；
通过百度客户端或底层 `baidu:apps/rcloneBaidu/encrypted-backups` 访问时看到密文。
这些密文不能由百度客户端直接打开解密。

## 密码与容量

加密密码及自定义盐需要独立备份。丢失配置且无法找回所用口令时，无法恢复数据。
配置中显示的 obscured 密码是可还原的遮蔽形式，并非安全的配置文件加密；
保护配置文件，必要时使用 rclone 自带的配置文件加密。

crypt 会增加少量文件大小。百度后端的分片数上限按加密后的大小计算，
因此默认分片设置下，接近 4 GiB 的明文文件可能因加密开销超出限制。
百度后端仍需要本地临时磁盘空间；加密发生在该后端接收数据之前。
文件大小和修改时间不被完全隐藏。

## 在其他设备恢复

不同设备的百度授权可以各自配置，但访问同一份加密备份必须使用完全相同的 crypt 密码、
自定义盐和名称加密设置，并指向同一个底层远端目录。
测试用密码未发布；请设置并妥善备份自己的密码。

实测结果见 [TESTING.md](TESTING.md)。
