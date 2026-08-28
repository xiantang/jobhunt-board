CREATE TABLE IF NOT EXISTS boards (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    key         TEXT    NOT NULL UNIQUE,
    name        TEXT    NOT NULL,
    description TEXT    NOT NULL DEFAULT '',
    created_at  TEXT    NOT NULL
);

CREATE TABLE IF NOT EXISTS members (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT    NOT NULL UNIQUE,
    role       TEXT    NOT NULL DEFAULT 'member' CHECK (role IN ('member', 'lead')),
    color      TEXT    NOT NULL DEFAULT '#6b7280',
    active     INTEGER NOT NULL DEFAULT 1,
    created_at TEXT    NOT NULL
);

-- stages 是可自由配置的面试阶段，按看板隔离。
-- key 是稳定标识（改名不动它），label 才是展示名。
CREATE TABLE IF NOT EXISTS stages (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    board_id       INTEGER NOT NULL REFERENCES boards (id) ON DELETE CASCADE,
    key            TEXT    NOT NULL,
    label          TEXT    NOT NULL,
    -- task: 任务阶段（在线测评这类），移进来挂一条只有 DDL 和链接的记录
    -- waiting: 等待回复列（面完了在等通知）。它不参与列顺序，任意阶段都能进出
    kind           TEXT    NOT NULL DEFAULT 'normal'
                   CHECK (kind IN ('normal', 'interview', 'task', 'waiting',
                                   'terminal_success', 'terminal_fail')),
    color          TEXT    NOT NULL DEFAULT '#6b7280',
    requires_owner INTEGER NOT NULL DEFAULT 0,
    -- skippable: 这一列允许被跨过去。中间隔着的列全部可跳过时，才准跨阶段流转。
    skippable      INTEGER NOT NULL DEFAULT 0,
    position       REAL    NOT NULL,
    created_at     TEXT    NOT NULL,
    UNIQUE (board_id, key)
);

CREATE INDEX IF NOT EXISTS idx_stages_board ON stages (board_id, position);

-- applications 是一条投递 / 一条面试流程，卡片即此表一行。
CREATE TABLE IF NOT EXISTS applications (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    board_id   INTEGER NOT NULL REFERENCES boards (id) ON DELETE CASCADE,
    seq        INTEGER NOT NULL,
    company    TEXT    NOT NULL,
    role       TEXT    NOT NULL DEFAULT '',
    channel    TEXT    NOT NULL DEFAULT '',
    notes      TEXT    NOT NULL DEFAULT '',
    stage_key  TEXT    NOT NULL,
    intent     TEXT    NOT NULL DEFAULT 'normal' CHECK (intent IN ('low', 'normal', 'high')),
    owner_id   INTEGER REFERENCES members (id) ON DELETE SET NULL,
    position   REAL    NOT NULL DEFAULT 0,
    created_at TEXT    NOT NULL,
    updated_at TEXT    NOT NULL,
    UNIQUE (board_id, seq),
    FOREIGN KEY (board_id, stage_key) REFERENCES stages (board_id, key) ON UPDATE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_applications_board ON applications (board_id, stage_key, position);

-- interview_rounds 是每一轮面试 / 每一个任务的独立记录。
-- stage_label 存快照，阶段改名后历史仍然可读。
-- google_accounts 存 Google Calendar 的授权凭证。
-- 这是一个人自己用的看板，所以只有一行（CHECK 把 id 钉死在 1）。
CREATE TABLE IF NOT EXISTS google_accounts (
    id            INTEGER PRIMARY KEY CHECK (id = 1),
    email         TEXT NOT NULL DEFAULT '',
    calendar_id   TEXT NOT NULL DEFAULT 'primary',
    access_token  TEXT NOT NULL DEFAULT '',
    -- refresh_token 是长期凭证，access_token 过期后靠它换新的
    refresh_token TEXT NOT NULL,
    expiry        TEXT NOT NULL DEFAULT '',
    connected_at  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS interview_rounds (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    application_id INTEGER NOT NULL REFERENCES applications (id) ON DELETE CASCADE,
    stage_key      TEXT    NOT NULL,
    stage_label    TEXT    NOT NULL,
    -- kind 决定 scheduled_at 的含义，也决定表单和日程上怎么画：
    --   interview 面试：scheduled_at 是开始时间，duration_min 是时长
    --   task      测评：scheduled_at 是截止时间（DDL），duration_min 无意义，
    --                  meeting_url 存的是测评链接
    kind           TEXT    NOT NULL DEFAULT 'interview'
                   CHECK (kind IN ('interview', 'task')),
    scheduled_at   TEXT,
    duration_min   INTEGER NOT NULL DEFAULT 60,
    mode           TEXT    NOT NULL DEFAULT 'online' CHECK (mode IN ('online', 'onsite', 'phone')),
    meeting_url    TEXT    NOT NULL DEFAULT '',
    meeting_place  TEXT    NOT NULL DEFAULT '',
    interviewer    TEXT    NOT NULL DEFAULT '',
    -- result: awaiting 是「面试已经结束、结果还没下来」的等待期，
    --         既不用再催时间，也还没有定论
    result         TEXT    NOT NULL DEFAULT 'pending'
                   CHECK (result IN ('pending', 'awaiting', 'passed', 'failed', 'cancelled')),
    notes          TEXT    NOT NULL DEFAULT '',
    -- google_event_id: 这一轮在 Google 日历上对应的事件，空表示还没同步过
    google_event_id TEXT   NOT NULL DEFAULT '',
    created_at     TEXT    NOT NULL,
    updated_at     TEXT    NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_rounds_app ON interview_rounds (application_id, scheduled_at);

CREATE TABLE IF NOT EXISTS application_events (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    application_id INTEGER NOT NULL REFERENCES applications (id) ON DELETE CASCADE,
    actor_id       INTEGER REFERENCES members (id) ON DELETE SET NULL,
    type           TEXT    NOT NULL,
    from_stage     TEXT,
    to_stage       TEXT,
    detail         TEXT    NOT NULL DEFAULT '',
    created_at     TEXT    NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_application_events_app ON application_events (application_id, id);
