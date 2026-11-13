# Bot API to MTProto Migration Plan

This document is a complete implementation plan for migrating the current
Telegram Bot API based verifier to MTProto while preserving behavior, tests,
and rollback safety.

The intended reader is an implementation agent or engineer. Follow the phases
in order. Do not skip the abstraction phase; the current business logic is
coupled to `go-telegram-bot-api` types, and a direct rewrite would make the
verification state machine difficult to validate.

## Goals

- Replace or optionally bypass Telegram Bot API operations with MTProto.
- Preserve all current user-facing behavior:
  - Join detection.
  - Restrict newly joined users.
  - Send verification image with inline keyboard.
  - Process callback answers in order.
  - Delete verification and join service messages on failure/timeout.
  - Unrestrict verified users.
  - Kick/unban first failure, permanent ban second failure.
  - Admin commands and test verification.
  - i18n and leaderboard behavior.
- Keep a working Bot API implementation during migration.
- Add enough tests that each phase can be merged independently.
- Make rollback a config change until MTProto is proven stable.

## Non-Goals

- Do not rewrite image composition, i18n, verification scoring, or existing
  database models unless needed for MTProto integration.
- Do not implement a userbot flow. Prefer MTProto bot login using bot token.
- Do not expose `access_hash` values in logs, admin commands, or user-facing
  output.
- Do not assume `access_hash` can be shared across accounts or sessions.

## Official References

- MTProto bots: https://core.telegram.org/api/bots
- Bot login: https://core.telegram.org/method/auth.importBotAuthorization
- Peer database and access hash: https://core.telegram.org/api/peers
- Bot API dialog ID mapping: https://core.telegram.org/api/bots/ids
- `InputPeer`: https://core.telegram.org/type/InputPeer
- `InputUser`: https://core.telegram.org/type/InputUser
- `channels.editBanned`: https://core.telegram.org/method/channels.editBanned
- `channels.deleteMessages`: https://core.telegram.org/method/channels.deleteMessages
- `messages.editMessage`: https://core.telegram.org/method/messages.editMessage

## Current State

Relevant files:

- `cmd/bot/main.go`: bootstraps config, database, migrations, and bot process.
- `internal/bot/bot.go`: creates Telegram client, starts update loop and cleanup
  goroutines.
- `internal/bot/telegram_api.go`: current narrow interface over
  `*tgbotapi.BotAPI`.
- `internal/bot/handlers.go`: message, command, chat member update handling,
  permissions, restrict/unrestrict.
- `internal/bot/verification.go`: verification lifecycle, callbacks, timeout
  handling, photo send, message edit/delete, file download.
- `internal/database/db.go`: SQLite schema and migrations embedded in code.
- `migrations/*.sql`: historical migrations.
- Tests under `internal/bot/*_test.go` and `internal/database/*_test.go`.

Current coupling:

- Business logic receives and emits `tgbotapi.Message`,
  `tgbotapi.CallbackQuery`, `tgbotapi.ChatMemberUpdated`,
  `tgbotapi.Chattable`, and concrete request configs.
- Existing tests mock `TelegramAPI` but still use Bot API types.
- This coupling must be removed before a clean MTProto adapter can be added.

## High-Level Architecture

Introduce a provider-neutral Telegram gateway layer:

```text
internal/bot
  business logic: verification state machine, admin commands, i18n
  depends on internal/telegram/domain + internal/telegram/gateway

internal/telegram/domain
  provider-neutral User, Chat, Message, CallbackQuery, MemberUpdate, ReplyMarkup

internal/telegram/botapi
  adapter around go-telegram-bot-api/v5

internal/telegram/mtproto
  adapter around gotd/td or TDLib

internal/database
  existing app data
  new peer cache + mtproto update/session state
```

Runtime modes:

- `TELEGRAM_BACKEND=botapi`: existing behavior, default during migration.
- `TELEGRAM_BACKEND=mtproto`: all supported operations through MTProto.
- `TELEGRAM_BACKEND=hybrid`: Bot API update loop plus MTProto admin operations.

Recommended rollout:

1. Ship `botapi` mode after abstraction with no behavior changes.
2. Add `hybrid` mode for high-value MTProto operations:
   `DeleteMessage`, `RestrictUser`, `BanUser`, `UnbanUser`.
3. Add full `mtproto` update loop and media/callback support.

## Library Choice

Preferred Go path: `github.com/gotd/td`.

Reasons:

- Pure Go, fits the repository.
- Supports MTProto for users and bots.
- Context-aware calls, generated API types, update support.
- Avoids TDLib C++ runtime and deployment complexity.

Alternative: TDLib.

Use TDLib only if `gotd/td` update/state management proves too costly. TDLib
is robust but would change Docker build/runtime significantly because it brings
C++ native dependencies and persistent TDLib database semantics.

## New Configuration

Add fields to `internal/config/config.go`:

- `TELEGRAM_BACKEND`, default `botapi`.
- `TELEGRAM_API_ID`, required for MTProto modes.
- `TELEGRAM_API_HASH`, required for MTProto modes.
- `MTPROTO_SESSION_PATH`, default `/data/mtproto.session`.
- `MTPROTO_DEVICE_MODEL`, default `ING-reCAPTCHA`.
- `MTPROTO_APP_VERSION`, default current git/version string if available.
- `MTPROTO_SYSTEM_LANG_CODE`, default `en`.
- `MTPROTO_LANG_PACK`, default empty.
- `MTPROTO_LANG_CODE`, default `en`.
- `TELEGRAM_DELETE_RETRY_MAX`, default `5`.
- `TELEGRAM_DELETE_RETRY_BASE_SECONDS`, default `2`.

Validation:

- In `botapi` mode, only `TELEGRAM_BOT_TOKEN` is required.
- In `mtproto` or `hybrid`, require bot token, API ID, API hash, and session
  path.
- Never log the bot token, API hash, session bytes, or access hashes.

## Database Additions

Create migration `migrations/006_add_mtproto_peer_cache.sql`, and mirror it in
`internal/database/db.go` embedded migration.

Tables:

```sql
CREATE TABLE IF NOT EXISTS telegram_peers (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    backend TEXT NOT NULL DEFAULT 'mtproto',
    peer_kind TEXT NOT NULL, -- user, chat, channel
    bot_api_id INTEGER NOT NULL,
    mtproto_id INTEGER NOT NULL,
    access_hash INTEGER,
    access_hash_kind TEXT NOT NULL DEFAULT 'unknown',
    is_min INTEGER NOT NULL DEFAULT 0,
    username TEXT,
    display_name TEXT,
    seen_in_bot_api_id INTEGER,
    seen_in_msg_id INTEGER,
    updated_at INTEGER NOT NULL,
    UNIQUE(backend, peer_kind, bot_api_id)
);

CREATE INDEX IF NOT EXISTS idx_telegram_peers_lookup
ON telegram_peers(backend, peer_kind, bot_api_id);

CREATE INDEX IF NOT EXISTS idx_telegram_peers_mtproto
ON telegram_peers(backend, peer_kind, mtproto_id);
```

```sql
CREATE TABLE IF NOT EXISTS mtproto_update_state (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    pts INTEGER NOT NULL DEFAULT 0,
    qts INTEGER NOT NULL DEFAULT 0,
    date INTEGER NOT NULL DEFAULT 0,
    seq INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL
);
```

```sql
CREATE TABLE IF NOT EXISTS telegram_delete_queue (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    chat_id INTEGER NOT NULL,
    message_id INTEGER NOT NULL,
    reason TEXT NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0,
    next_attempt_at INTEGER NOT NULL,
    last_error TEXT,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE(chat_id, message_id, reason)
);

CREATE INDEX IF NOT EXISTS idx_delete_queue_due
ON telegram_delete_queue(next_attempt_at, attempts);
```

Why add `telegram_delete_queue`:

- Existing timeout handling may delete the DB pending record while Telegram
  `deleteMessage` fails transiently.
- Persisting delete retries prevents stale verification messages from remaining
  indefinitely after network/API failures.
- This queue is useful for both Bot API and MTProto.

Database tests:

- Peer upsert preserves a full access hash over min/unknown values.
- Peer lookup converts Bot API IDs to peer records.
- Delete queue inserts idempotently.
- Delete queue due selection respects `next_attempt_at`.
- Delete queue marks success and removes records.
- Delete queue marks failure and schedules exponential retry.

## Domain Model

Create package `internal/telegram/domain` or `internal/telegram` with small
provider-neutral types.

Suggested types:

```go
type Backend string

const (
    BackendBotAPI   Backend = "botapi"
    BackendMTProto  Backend = "mtproto"
    BackendHybrid   Backend = "hybrid"
)

type ChatType string

const (
    ChatPrivate    ChatType = "private"
    ChatGroup      ChatType = "group"
    ChatSupergroup ChatType = "supergroup"
    ChatChannel    ChatType = "channel"
)

type User struct {
    ID        int64
    IsBot     bool
    UserName  string
    FirstName string
    LastName  string
    LangCode  string
}

type Chat struct {
    ID       int64
    Type     ChatType
    Title    string
    UserName string
}

type Message struct {
    ID             int
    Chat           Chat
    From           *User
    Text           string
    Command        string
    CommandArgs    string
    NewChatMembers []User
    ReplyToPhoto   *Photo
}

type CallbackQuery struct {
    ID      string
    From    User
    Message Message
    Data    string
}

type MemberStatus string

const (
    StatusCreator       MemberStatus = "creator"
    StatusAdministrator MemberStatus = "administrator"
    StatusMember        MemberStatus = "member"
    StatusRestricted    MemberStatus = "restricted"
    StatusLeft          MemberStatus = "left"
    StatusKicked        MemberStatus = "kicked"
)

type MemberUpdate struct {
    Chat      Chat
    User      User
    OldStatus MemberStatus
    NewStatus MemberStatus
}

type Update struct {
    Message      *Message
    Callback     *CallbackQuery
    MemberUpdate *MemberUpdate
}
```

Suggested gateway:

```go
type Gateway interface {
    Self(ctx context.Context) (User, error)
    Updates(ctx context.Context) (<-chan Update, error)
    Stop(ctx context.Context) error

    SendText(ctx context.Context, chatID int64, text SendText) (SentMessage, error)
    SendPhoto(ctx context.Context, chatID int64, photo SendPhoto) (SentMessage, error)
    EditMessageCaption(ctx context.Context, chatID int64, messageID int, edit EditCaption) error
    EditMessageText(ctx context.Context, chatID int64, messageID int, edit EditText) error
    AnswerCallback(ctx context.Context, callbackID string, text string) error

    DeleteMessage(ctx context.Context, chatID int64, messageID int) error
    GetChatMember(ctx context.Context, chatID, userID int64) (ChatMember, error)
    RestrictUser(ctx context.Context, chatID, userID int64, permissions ChatPermissions) error
    UnrestrictUser(ctx context.Context, chatID, userID int64, permissions ChatPermissions) error
    BanUser(ctx context.Context, chatID, userID int64, untilUnix int64) error
    UnbanUser(ctx context.Context, chatID, userID int64, onlyIfBanned bool) error
    GetFile(ctx context.Context, fileID string) (RemoteFile, error)
    DownloadFile(ctx context.Context, file RemoteFile, dstPath string) error
}
```

Business code should not import `tgbotapi` after this phase except in adapter
packages and tests explicitly targeting the Bot API adapter.

## Bot API Adapter Phase

Purpose: prove the abstraction without changing runtime behavior.

Tasks:

1. Create `internal/telegram/botapi`.
2. Move all `tgbotapi` conversion logic into that package.
3. Keep `go-telegram-bot-api/v5` as the backing client.
4. Translate Bot API updates into domain `Update`.
5. Translate domain send/edit/delete/restrict calls into `tgbotapi` configs.
6. Refactor `internal/bot` to depend on `Gateway`.
7. Keep tests green.

Acceptance criteria:

- `go test ./...` passes.
- `TELEGRAM_BACKEND=botapi` behavior matches current production behavior.
- `internal/bot` no longer imports `github.com/go-telegram-bot-api/telegram-bot-api/v5`
  except where migration cannot be avoided; any remaining imports must be
  documented.

Suggested commit:

- `refactor: introduce telegram gateway abstraction`

## Delete Retry Queue Phase

Purpose: fix stale verification messages independently of MTProto.

Tasks:

1. Add database migration for `telegram_delete_queue`.
2. Add database methods:
   - `EnqueueDeleteMessage(chatID, messageID, reason string) error`
   - `GetDueDeleteMessages(now int64, limit int) ([]DeleteTask, error)`
   - `MarkDeleteMessageSucceeded(id int64) error`
   - `MarkDeleteMessageFailed(id int64, lastErr string, nextAttemptAt int64) error`
3. Wrap all verification-message and join-message deletion calls:
   - On success: no queue entry.
   - On transient failure: enqueue.
   - On known non-retryable error (`message to delete not found`, permission
     denied after bot removal): mark completed or drop with log.
4. Add background cleanup worker every 10 seconds, bounded concurrency.
5. Share this queue for Bot API and later MTProto.

Acceptance criteria:

- If `DeleteMessage` fails in tests, a queue entry is created.
- Retry worker eventually removes queued entry when mock gateway succeeds.
- Pending verification DB cleanup remains independent from message delete
  success.
- Existing timeout behavior still removes `pending_verifications`.

Suggested commit:

- `fix: persist and retry telegram message deletes`

## MTProto Peer Cache Phase

Purpose: store and resolve MTProto peers safely.

Tasks:

1. Add migration for `telegram_peers` and `mtproto_update_state`.
2. Implement `internal/database/telegram_peer.go`.
3. Implement Bot API ID conversion helpers.
4. Persist peers whenever MTProto updates or RPC responses include users/chats.
5. Store access hash priority:
   - `full` wins over `from_message`.
   - `from_message` wins over `min`.
   - `min` wins over `zero`.
   - Never overwrite a higher-priority hash with a lower-priority one.
6. Store context fields for `inputPeerUserFromMessage` /
   `inputPeerChannelFromMessage` when only min peers are available.

Acceptance criteria:

- Unit tests cover peer upsert priority.
- Unit tests cover Bot API ID conversion for users, basic groups, and
  supergroups/channels.
- No access hash appears in logs.

Suggested commit:

- `feat: add mtproto peer cache`

## MTProto Client Phase

Purpose: establish authenticated MTProto connection as the bot.

Tasks:

1. Add dependency `github.com/gotd/td`.
2. Add `internal/telegram/mtproto/client.go`.
3. Use `auth.importBotAuthorization` with:
   - `TELEGRAM_API_ID`
   - `TELEGRAM_API_HASH`
   - `TELEGRAM_BOT_TOKEN`
4. Store session at `MTPROTO_SESSION_PATH`.
5. Support graceful shutdown via `context.Context`.
6. Add structured error classification:
   - flood wait
   - peer invalid
   - channel private
   - admin required
   - message delete forbidden
   - transient network

Acceptance criteria:

- Unit tests can construct MTProto gateway with fake RPC invoker.
- No integration test requires real Telegram credentials unless explicitly
  gated by env vars.
- Missing MTProto config fails during startup with a clear error.

Suggested commit:

- `feat: add mtproto bot client bootstrap`

## Hybrid Admin Operations Phase

Purpose: reduce Bot API dependency for operations that currently cause the most
pain, while keeping Bot API updates and callback handling.

Implement MTProto-backed methods:

- `DeleteMessage`:
  - Supergroup/channel: `channels.deleteMessages`.
  - Basic group/private: use the appropriate messages delete method.
  - Use peer cache to resolve `InputChannel` or `InputPeer`.
- `RestrictUser`:
  - Supergroup/channel: `channels.editBanned` with send/media/etc rights denied.
- `UnrestrictUser`:
  - `channels.editBanned` with rights cleared/restored.
- `BanUser`:
  - `channels.editBanned` with view/send rights denied and optional until date.
- `UnbanUser`:
  - `channels.editBanned` with restrictions cleared.
- `GetChatMember`:
  - Use channel participant APIs where possible.
  - Match existing behavior only to the extent needed:
    admin/creator checks, bot permission checks.

Tasks:

1. Add `TELEGRAM_BACKEND=hybrid`.
2. Compose gateway:
   - Bot API handles updates, send photo, edit caption, callbacks, file get.
   - MTProto handles delete/restrict/ban/unban where possible.
   - Fall back to Bot API when MTProto peer resolution fails, with warning.
3. Persist peers from any MTProto RPC responses.
4. Use delete retry queue for both backends.

Acceptance criteria:

- Production can run in `botapi` or `hybrid` with the same database.
- If MTProto delete fails because peer is missing, fallback happens and a peer
  resolution warning is logged without access hash.
- Unit tests cover fallback.

Suggested commit:

- `feat: support hybrid mtproto moderation operations`

## Full MTProto Update Loop Phase

Purpose: remove dependency on Bot API long polling.

Tasks:

1. Implement MTProto update dispatcher.
2. Persist update state (`pts`, `qts`, `date`, `seq`) as required by library
   or adapter.
3. Convert MTProto updates into domain `Update`:
   - new message
   - service message with joined users
   - callback query / bot callback
   - chat member status changes
   - edited messages if needed for callbacks
4. Ensure privacy-mode behavior is understood and documented.
5. Handle gaps and resync:
   - use gotd update manager if available.
   - otherwise implement difference fetching and state persistence.
6. Add bounded worker pool same as current update handling.

Acceptance criteria:

- Bot receives `/test`, `/help`, `/myid`, admin commands in MTProto mode.
- New member join triggers verification in MTProto mode.
- Callback answers progress verification.
- Restart does not replay already handled updates.

Suggested commit:

- `feat: add mtproto update loop`

## MTProto Send/Edit/Callback/File Phase

Purpose: complete MTProto backend parity.

Implement:

- `SendText`: `messages.sendMessage`.
- `SendPhoto`: upload composed image, send media with caption and inline
  keyboard.
- `EditMessageCaption`: edit media caption/markup.
- `EditMessageText`: edit leaderboard message.
- `AnswerCallback`: MTProto bot callback answer method.
- `GetFile` and `DownloadFile`:
  - For historical images stored only by Bot API `file_id`, decide whether
    to keep Bot API download fallback or recache images from MTProto objects.
  - Short term: keep Bot API file download fallback in `hybrid`.
  - Full MTProto: store enough MTProto file location metadata when images are
    added.

Important file compatibility issue:

- Existing `images.file_id` values are Bot API file IDs. They are not directly
  usable as MTProto file locations.
- For full MTProto, add a new table or columns for MTProto file metadata:
  `dc_id`, `file_reference`, `volume_id`, `local_id`, `document_id`,
  `access_hash`, `file_kind`, etc., depending on actual media constructors.
- Until that is implemented, keep Bot API file download support or require
  local cached images to exist.

Acceptance criteria:

- `/addimage` remains compatible.
- Existing cached images continue to work.
- Verification can be generated using existing cached files.
- If uncached Bot API-only file IDs exist in full MTProto mode, startup health
  check reports clear remediation or hybrid download is enabled.

Suggested commit:

- `feat: complete mtproto message and media operations`

## Business Logic Refactor Details

Refactor in small steps:

1. Change method signatures:
   - `handleMessage(*tgbotapi.Message)` to `handleMessage(*telegram.Message)`.
   - `handleCallbackQuery(*tgbotapi.CallbackQuery)` to domain callback.
   - `handleChatMemberUpdate(*tgbotapi.ChatMemberUpdated)` to domain member
     update.
2. Replace `tgbotapi.NewMessage`, `NewPhoto`, `NewDeleteMessage`, etc. with
   gateway calls.
3. Move keyboard construction to provider-neutral `InlineKeyboard`.
4. Adapter packages render provider-specific keyboard types.
5. Keep `formatUserMention` provider-neutral and ensure HTML/entity handling is
   adapter-aware.

Entity handling:

- Bot API uses HTML parse mode.
- MTProto should prefer explicit message entities where feasible.
- Do not blindly reuse HTML in MTProto if the library expects entities.
- Add tests for mentions, bold text, labels containing special chars, and
  multilingual labels.

## Testing Strategy

Run `go test ./...` after every phase.

### Existing tests to keep passing

- `internal/bot/*_test.go`
- `internal/database/*_test.go`
- `internal/i18n/*_test.go`
- `internal/imaging/*_test.go`

### New unit tests

Gateway domain:

- Bot API message conversion:
  - command parsing.
  - private/group/supergroup chat type mapping.
  - new chat members.
  - callback data.
  - member status update.

- Keyboard conversion:
  - button text.
  - callback data.
  - row layout.
  - invalid/oversized callback data handling.

- Permission conversion:
  - restrict.
  - unrestrict.
  - ban.
  - temporary ban.

Peer cache:

- Upsert full hash.
- Full hash not overwritten by min hash.
- From-message context stored.
- Bot API ID conversion.
- Missing peer returns typed error.

Delete queue:

- Enqueue idempotency.
- Retry scheduling.
- Success removal.
- Non-retryable errors.

MTProto adapter:

- Fake RPC invoker verifies correct methods:
  - `channels.editBanned`.
  - `channels.deleteMessages`.
  - message send/edit.
- Flood wait maps to retryable typed error.
- Admin required maps to permission typed error.
- Peer invalid maps to peer resolution typed error.

Business behavior with fake gateway:

- New member creates pending verification and sends photo.
- Pending user message is deleted/enqueued on failure.
- Expired verification claims record and attempts delete/kick.
- Delete failure enqueues retry but pending record is removed.
- User removed by another anti-spam bot clears pending state.
- `/test` still works in group.

### Integration tests

Keep integration tests opt-in:

- `TELEGRAM_INTEGRATION=1`
- `TELEGRAM_TEST_CHAT_ID`
- `TELEGRAM_BOT_TOKEN`
- `TELEGRAM_API_ID`
- `TELEGRAM_API_HASH`

Integration scenarios:

- Connect MTProto session.
- Resolve test chat peer.
- Send and delete a test message.
- Restrict/unrestrict a controlled test account only if explicitly enabled.
- Never run moderation integration tests by default.

## Code Management Plan

Use a feature branch:

```bash
git checkout -b feat/mtproto-gateway
```

Recommended commits:

1. `docs: add mtproto migration plan`
2. `refactor: introduce telegram gateway abstraction`
3. `test: cover gateway domain conversions`
4. `fix: persist and retry telegram message deletes`
5. `feat: add mtproto peer cache`
6. `feat: add mtproto bot client bootstrap`
7. `feat: support hybrid mtproto moderation operations`
8. `feat: add mtproto update loop`
9. `feat: complete mtproto message and media operations`
10. `docs: document mtproto deployment and rollback`

Rules:

- Keep each commit testable.
- Do not mix schema migration, business logic refactor, and MTProto adapter
  implementation in one commit.
- Keep `TELEGRAM_BACKEND=botapi` working until the final cutover.
- Keep Docker image building after every merge.
- Avoid rewriting history once deployed.

## Deployment Plan

Phase A: Bot API abstraction only.

- Deploy with `TELEGRAM_BACKEND=botapi`.
- Confirm no behavior change.

Phase B: Delete retry queue.

- Deploy with `TELEGRAM_BACKEND=botapi`.
- Watch queue size and stale verification messages.

Phase C: Hybrid moderation.

- Set:
  - `TELEGRAM_BACKEND=hybrid`
  - `TELEGRAM_API_ID`
  - `TELEGRAM_API_HASH`
  - `MTPROTO_SESSION_PATH=/data/mtproto.session`
- Watch logs for peer resolution failures and fallback rates.
- Keep fallback enabled.

Phase D: Full MTProto in staging chat.

- Use a separate test bot or test group.
- Run integration tests.
- Verify join, callback, timeout, and cleanup.

Phase E: Production full MTProto.

- Set `TELEGRAM_BACKEND=mtproto`.
- Keep Bot API fallback feature flag available for file download and emergency
  message deletion until stable.

## Rollback Plan

Rollback must be possible by configuration:

- Change `TELEGRAM_BACKEND=botapi`.
- Restart container.
- Keep MTProto tables; they are additive and should not affect Bot API mode.
- If delete retry queue is active, Bot API mode should continue processing it.

Do not remove old Bot API code until MTProto has run in production long enough
to cover:

- normal join verification.
- test verification.
- first failure.
- second failure.
- timeout.
- user removed by another anti-spam bot.
- Telegram API transient failures.
- process restart during pending verification.

## Observability

Add structured logs for:

- backend selected.
- MTProto login/session path initialized.
- peer cache miss/hit, without access hash.
- update gap/resync.
- delete queue enqueue/success/failure.
- moderation action success/failure.
- fallback from MTProto to Bot API.
- flood wait duration.

Optional metrics if a metrics system exists later:

- `telegram_updates_total{backend,type}`
- `telegram_rpc_errors_total{backend,method,kind}`
- `telegram_delete_queue_size`
- `telegram_delete_retries_total`
- `verification_started_total`
- `verification_expired_total`
- `verification_passed_total`
- `verification_failed_total`

## Security Notes

- Treat MTProto session files as secrets.
- Store `/data/mtproto.session` only in persistent private storage.
- Never log bot token, API hash, access hash, file references, or session data.
- Access hashes are session/account scoped; never copy them between bots,
  accounts, or environments.
- Prefer typed errors over logging raw serialized Telegram objects.

## Acceptance Criteria For Full Migration

The migration is complete only when all conditions below are true:

- `TELEGRAM_BACKEND=botapi` passes all tests and can still run.
- `TELEGRAM_BACKEND=hybrid` passes unit tests and integration smoke tests.
- `TELEGRAM_BACKEND=mtproto` passes unit tests and integration smoke tests.
- Existing cached image sets can generate verification challenges.
- New images can be added and reused.
- Expired verification records are removed from DB.
- Expired verification messages are deleted or queued for retry.
- User removed by another anti-spam bot clears local pending state.
- Join service message handling remains correct.
- `/test`, `/setstats`, `/listsets`, `/addimage`, `/setlang`, and leaderboard
  operations work.
- Docker image builds successfully.
- Rollback to Bot API mode requires no schema rollback.

## Suggested First Implementation Task

Start with the gateway abstraction, not MTProto.

Concrete first PR:

1. Add provider-neutral domain types.
2. Add `Gateway` interface.
3. Implement `botapi.Gateway`.
4. Refactor `internal/bot` to use the interface.
5. Keep all existing tests green.
6. Add conversion tests for Bot API adapter.

This creates a clean boundary so MTProto can be added as an adapter instead of
being threaded through the verification state machine.
