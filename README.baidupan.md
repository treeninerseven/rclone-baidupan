# rclone 百度网盘实验后端

本仓库基于 [rclone/rclone](https://github.com/rclone/rclone)，新增 `baidupan` 后端。
支持目录列表、下载、分片上传、同名覆盖、目录创建、文件及空目录删除、令牌自动刷新，
以及通过 rclone 内置 `crypt` 加密备份。

这是个人实验分支，尚未合并到 rclone 上游。上游正式版二进制及 Docker 镜像不包含此后端。
保留上游源码、Git 历史和 MIT 许可证，改动集中在 `backend/baidupan` 及其注册、测试和文档中。

## 在其他设备编译

需要 Git 和 Go **1.26.0 或更新的兼容版本**；当前验证使用 Go 1.26.10。
从 [Go 官网](https://go.dev/dl/) 安装适合目标设备的工具链，然后执行：

```sh
git clone --branch codex/baidupan https://github.com/treeninerseven/rclone-baidupan.git
cd rclone-baidupan
go build -trimpath -ldflags "-s -w -X github.com/rclone/rclone/fs.Version=v1.76.0-baidupan-experimental" -o rclone .
./rclone version
./rclone help backend baidupan
```

这会为当前设备的操作系统和 CPU 编译。Linux 和 macOS 使用 `./rclone`；
Windows 将构建命令的输出改为 `-o rclone.exe`，之后使用 `.\rclone.exe`。
可把生成的二进制放入自己的 PATH。初次编译需要下载 Go 模块。

也可以在有 Go 的设备上交叉编译 Linux 版本，例如：

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o rclone-linux-amd64 .
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o rclone-linux-arm64 .
```

将对应架构的文件传到目标设备后赋予执行权限。上述交叉编译关闭 CGO，
某些依赖 CGO 的可选挂载功能可能不可用；百度后端和 crypt 不依赖这些功能。

## 配置百度网盘

在 [百度网盘开放平台](https://pan.baidu.com/union) 创建自己的应用，取得 AppKey 和 SecretKey。
不需要把任何密钥写进源码。每台设备可单独执行授权，配置保存在本地：

```sh
mkdir -p .local
./rclone --config .local/rclone.conf config
```

新建 remote，名称填写 `baidu`，存储类型直接输入 `baidupan`。
`client_id` 填 AppKey，`client_secret` 选择自行输入并填 SecretKey。
按照终端给出的网址在浏览器完成授权，将授权码填回终端；无浏览器的服务器可以在另一台设备打开网址。
保存后验证：

```sh
./rclone --config .local/rclone.conf lsf baidu:
chmod 600 .local/rclone.conf
```

访问令牌会自动刷新并写回配置，因此运行账号必须有权限写入配置文件及其所在目录。
配置包含可用于访问网盘的凭据，不要上传到公开仓库或放入镜像。
建议各设备单独授权；不要让多个设备共用一个持续同步的可写配置文件。

## 备份与恢复

将 `rcloneBaidu` 替换为你自己的应用名称：

```sh
./rclone --config .local/rclone.conf copy ./documents \
  baidu:apps/rcloneBaidu/backups/documents --create-empty-src-dirs --progress

./rclone --config .local/rclone.conf check ./documents \
  baidu:apps/rcloneBaidu/backups/documents --download

./rclone --config .local/rclone.conf copy \
  baidu:apps/rcloneBaidu/backups/documents ./restored --create-empty-src-dirs --progress
```

`copy` 保留远端多余文件，同名覆盖替换旧内容，不自动保存历史版本。
已实测的写入范围是应用目录 `/apps/应用名称/`。
完整说明见 [普通备份](docs/baidupan/BACKUP.md) 和 [加密备份](docs/baidupan/ENCRYPTION.md)。

## 当前验证与限制

真实账号已验证授权、刷新、分页、下载及分段读取、8 MiB 多分片备份和恢复、
同大小内容更新、空文件、空目录、中文及特殊名称。
crypt 测试包含加密名称、多分片上传、完整恢复 SHA-256 比对、分段解密、错误密码及篡改检测。
模拟服务 race 测试通过，后端语句覆盖率为 83.1%；标准后端短模式集成测试通过。
详见 [测试说明](docs/baidupan/TESTING.md)。

- 默认 4 MiB 分片，当前实现限制每文件最多 1024 片，即默认 4 GiB；crypt 按加密后的大小计算。
- 16/32 MiB 分片需要相应百度账号权限，多 GiB 文件尚未实测。
- 上传需要本地临时磁盘容纳完整文件；并发上传会同时占用多个文件的空间。
- 暂无跨进程上传续传、服务端复制/移动、公共分享链接支持。
- 不绕过百度应用权限、账号限速或平台限制。

该分支基于上游提交 `81e7372f6fbedfa80592e9a564593d63a8dc32f3`，
是源码快照，并非上游 `v1.76.0` 的发布标签。
