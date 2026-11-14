# Ingress 入群验证 Bot 使用文档

## 目录

1. [概述](#概述)
2. [快速开始](#快速开始)
3. [配置说明](#配置说明)
4. [权限管理](#权限管理)
5. [命令参考](#命令参考)
6. [图片集管理](#图片集管理)
7. [验证流程](#验证流程)
8. [部署指南](#部署指南)
9. [故障排除](#故障排除)

---

## 概述

这是一个用于 Ingress 主题 Telegram 群组的入群验证 Bot。新成员加入时需要通过图片识别验证才能获得发言权限。

### 主要功能

- 新成员自动触发验证
- 图片识别验证（从预设图片集中随机选择）
- 防 Hash 扫描的图片合成
- 可配置的验证参数
- 防刷机制（重复加入冷却）
- 跨群免验证（在任一群自行通过验证后，加入其他群不再验证）
- 管理员权限管理

---

## 快速开始

### 1. 创建 Telegram Bot

1. 在 Telegram 中找到 [@BotFather](https://t.me/BotFather)
2. 发送 `/newbot` 创建新 Bot
3. 按提示设置 Bot 名称和用户名
4. 保存获得的 Bot Token

### 2. 获取你的 User ID

1. 启动 Bot 后，私聊发送 `/myid`
2. 记录返回的数字 ID

### 3. 配置环境变量

```bash
cp .env.example .env
```

编辑 `.env` 文件：

```env
TELEGRAM_BOT_TOKEN=你的Bot_Token
BOT_ADMIN_IDS=你的User_ID
```

### 4. 启动 Bot

```bash
# 使用 Docker Compose
docker-compose up -d --build

# 或本地运行
go build -o bot ./cmd/bot
./bot
```

### 5. 配置 Bot 权限

将 Bot 添加到群组并设为管理员，需要以下权限：
- ✅ 删除消息
- ✅ 封禁用户
- ✅ 限制成员

### 6. 添加图片集

至少需要 **9 个不同的图片集**，每个集合至少 1 张图片。

```
/addset Portal
/addset Resonator
/addset XMP
/addset UltraStrike
/addset PowerCube
/addset Shield
/addset LinkAmp
/addset HeatSink
/addset MultiHack
```

然后为每个集合添加图片（回复图片发送命令）：

```
/addimage Portal
```

---

## 配置说明

### 环境变量

| 变量名 | 必填 | 默认值 | 说明 |
|--------|------|--------|------|
| `TELEGRAM_BOT_TOKEN` | ✅ | - | Telegram Bot Token |
| `BOT_ADMIN_IDS` | 推荐 | - | Bot 管理员 ID，逗号分隔 |
| `DATABASE_PATH` | ❌ | `/data/bot.db` | SQLite 数据库路径 |
| `IMAGE_CACHE_PATH` | ❌ | `/images` | 图片缓存目录 |
| `VERIFY_IMAGE_COUNT` | ❌ | `3` | 验证时显示的图片数量 |
| `VERIFY_REQUIRED_CORRECT` | ❌ | `2` | 需要答对的数量 |
| `VERIFY_TIMEOUT_SECONDS` | ❌ | `120` | 验证超时时间（秒） |
| `VERIFY_MAX_RETRY` | ❌ | `1` | 最大重试次数 |
| `REJOIN_COOLDOWN_SECONDS` | ❌ | `300` | 重新加入冷却时间（秒） |

### 验证参数说明

- **图片数量 (n=3)**：验证时显示 3 张图片
- **通过条件 (y=2)**：答对 2 个即可通过
- **选项数量**：9 个选项（3 个正确 + 6 个干扰）
- **超时时间**：120 秒内未完成验证将被踢出
- **重试次数**：失败后可重试 1 次

---

## 权限管理

### 权限层级

Bot 有两种权限检查机制：

#### 1. Bot 管理员（全局）

- 通过 `BOT_ADMIN_IDS` 环境变量配置
- 通过 `/addadmin` 命令动态添加
- 可以在**私聊**中管理图片集
- 可以管理其他 Bot 管理员

#### 2. 群组管理员（群组内）

- Telegram 群组的管理员/群主
- 可以在**群组内**管理图片集
- 不能管理 Bot 管理员

### 配置初始管理员

**方法一：环境变量（推荐）**

在 `.env` 文件中设置：

```env
BOT_ADMIN_IDS=123456789,987654321
```

多个管理员用逗号分隔。

**方法二：命令添加**

首先需要有一个初始管理员（通过环境变量设置），然后可以用命令添加更多：

```
/addadmin 123456789
```

### 获取 User ID

1. 私聊 Bot 发送 `/myid`
2. 或使用 [@userinfobot](https://t.me/userinfobot)

### 管理员命令

| 命令 | 说明 | 权限要求 |
|------|------|----------|
| `/addadmin <user_id>` | 添加 Bot 管理员 | Bot 管理员 |
| `/removeadmin <user_id>` | 移除 Bot 管理员 | Bot 管理员 |
| `/listadmins` | 列出所有 Bot 管理员 | Bot 管理员 |

---

## 命令参考

### 通用命令

| 命令 | 说明 |
|------|------|
| `/start` | 显示欢迎信息 |
| `/help` | 显示帮助信息 |
| `/myid` | 获取你的 Telegram User ID |

### 图片集管理命令

| 命令 | 说明 | 权限要求 |
|------|------|----------|
| `/addset <label>` | 创建新图片集 | 管理员 |
| `/addimage <label>` | 添加图片到指定集合 | 管理员 |
| `/listsets` | 列出所有图片集 | 管理员 |
| `/delset <label>` | 删除图片集 | 管理员 |
| `/setstats` | 查看各集合图片数量 | 管理员 |
| `/test` | 触发测试验证 | 管理员 |

### Bot 管理员命令

| 命令 | 说明 | 权限要求 |
|------|------|----------|
| `/addadmin <user_id>` | 添加 Bot 管理员 | Bot 管理员 |
| `/removeadmin <user_id>` | 移除 Bot 管理员 | Bot 管理员 |
| `/listadmins` | 列出所有 Bot 管理员 | Bot 管理员 |

---

## 图片集管理

### 创建图片集

```
/addset Portal
```

### 添加图片

1. 在群组或私聊中发送一张图片
2. **回复**该图片，发送命令：

```
/addimage Portal
```

### 查看统计

```
/setstats
```

输出示例：
```
📊 图片集统计

• Portal: 5 张
• Resonator: 3 张
• XMP: 4 张

共 3 个图片集，12 张图片

⚠️ 需要至少 9 个图片集才能正常验证
```

### 删除图片集

```
/delset Portal
```

⚠️ 这会删除该集合下的所有图片。

### 最低要求

- 至少 **9 个不同的图片集**
- 每个集合至少 **1 张图片**
- 建议每个集合 **3-5 张图片**以增加随机性

---

## 验证流程

### 用户视角

1. 用户加入群组
2. 立即被限制发言权限
3. 收到验证消息，显示 3 张合成图片
4. 依次为图片 1、2、3 选择正确的标签
5. 答对 2 个以上：验证通过，恢复权限
6. 答错：
   - 首次失败：重新生成验证
   - 再次失败：被踢出群组
7. 超时未完成：被踢出群组

### 跨群免验证

用户只要在 Bot 所在的任意一个群里**自己答对**过验证（群内验证或 Mini App 入群验证均可），就会被记入全局通过名单（`verified_users` 表）。之后加入其他由该 Bot 管理的群时：

- 普通入群：不限制发言、不发验证消息，直接成为正常成员
- 申请入群（Join Request）：Bot 直接批准申请，不弹出 Mini App

以下情况**不会**记入通过名单，用户下次入群仍需验证：

- 管理员点击「通过」按钮放行，或管理员手动批准入群申请 / 直接拉人进群
- 图片集不足时的自动放行、入群申请超时后按 `/joinpolicy` 放行
- `/test` 测试验证

升级到带有此功能的版本时，历史记录中自行通过验证的用户会被一次性导入名单。

### 管理员放行免验证

本群管理员批准某人的入群申请，或直接把人拉进群，等同于在验证消息上点「通过」：Bot 不禁言、不发验证消息，直接放行。

- 只认**本群管理员**（含匿名管理员）的操作；普通成员拉人、其他机器人批准的，仍然需要验证
- 这类成员不会记入通过名单，之后加入其他群仍需验证
- 如果 Bot 当时正在用 Mini App 验证这条申请，该验证会随之结束，不会再按 `/joinpolicy` 处理

### 私密验证（Ephemeral）的自动回退

开启 `/setting ephemeral on`（或 `EPHEMERAL_VERIFY_ENABLED=true`）后，验证消息只有入群者本人可见。但 Telegram 不保证这类消息一定送达，用户离线时尤其如此——例如管理员批准入群申请时，申请人往往并不在线。

因此，私密验证消息发出后如果 `EPHEMERAL_FALLBACK_SECONDS`（默认 30 秒）内入群者一次按钮都没有点，Bot 会改发一条普通的群内验证消息（@ 该用户，重新计时），并撤回原来的私密消息。只要用户点过任何按钮，就说明私密消息已送达，不会回退。

- 设为 `0` 可关闭回退
- 代价：始终不响应的入群者（多为广告号）在宽限期后仍会产生一条公开验证消息

### 自动删除“验证通过”消息

用户通过验证后，Bot 会在群里发一条“验证通过”提示（自行答对、管理员点「通过」、Mini App 入群验证通过，三种情况都会发）。群管理员可以让这条提示在一段时间后自动删除：

```
/setting autodelete 30      # 30 秒后删除
/setting autodelete 5m      # 也可以带单位：30s、5m、1h
/setting autodelete off     # 关闭（默认），提示一直保留
/setting                    # 查看当前设置
```

- 仅本群管理员可设置，按群生效；允许范围 5 秒 ~ 24 小时
- 只影响设置之后发出的提示，已经在群里的不会被删除
- 待删除的消息记录在数据库中，Bot 重启不会漏删

### 验证消息示例

```
👋 欢迎 张三！

请在 120 秒内完成验证。
图片中显示了 3 张图片（标注 1、2、3）。

请依次为每张图片选择正确的标签。

当前：请选择图片 1 的标签

[Portal] [Resonator] [XMP]
[Shield] [LinkAmp] [HeatSink]
[PowerCube] [UltraStrike] [MultiHack]
```

---

## 部署指南

### Docker 部署（推荐）

```bash
# 1. 克隆项目
git clone <repo_url>
cd ING-reCAPTCHA

# 2. 配置环境变量
cp .env.example .env
# 编辑 .env 文件

# 3. 启动
docker-compose up -d --build

# 4. 查看日志
docker-compose logs -f
```

### 本地运行

```bash
# 1. 安装 Go 1.21+
# 2. 安装依赖
go mod download

# 3. 配置环境变量
export TELEGRAM_BOT_TOKEN=your_token
export BOT_ADMIN_IDS=your_user_id

# 4. 运行
go run ./cmd/bot
```

### 数据持久化

Docker 部署时，以下目录会被挂载：

- `./data/` - SQLite 数据库
- `./images/` - 图片缓存

---

## 故障排除

### Bot 无响应

1. 检查 Bot Token 是否正确
2. 检查网络连接
3. 查看日志：`docker-compose logs -f`

### 验证不触发

1. 确保 Bot 是群组管理员
2. 确保 Bot 有"限制成员"权限
3. 检查是否有足够的图片集（至少 9 个）

### 权限不足

1. 群组内：确保你是群组管理员
2. 私聊中：确保你的 User ID 在 `BOT_ADMIN_IDS` 中

### 图片无法添加

1. 确保回复的是图片消息
2. 确保图片集已创建
3. 检查磁盘空间

### 获取 User ID

```
/myid
```

或使用 [@userinfobot](https://t.me/userinfobot)

---

## 技术支持

如有问题，请提交 Issue 或联系管理员。
