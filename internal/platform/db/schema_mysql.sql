-- MySQL 版表结构，和 schema_sqlite.sql 一一对应。差异只在类型和索引写法上：
--   INTEGER PRIMARY KEY AUTOINCREMENT -> BIGINT AUTO_INCREMENT
--   REAL                              -> DOUBLE
--   布尔的 INTEGER 0/1                -> TINYINT 0/1
--   时间的 TEXT                       -> VARCHAR(40)，仍然是 RFC3339 字符串
--   带 DEFAULT '' 的 TEXT             -> VARCHAR(n)，MySQL 的 TEXT 不能有字面默认值
--   CREATE INDEX IF NOT EXISTS        -> 写进建表语句里的 KEY，跟着表一起幂等
-- "key" 在 MySQL 里是保留字，必须带引号。用双引号而不是反引号：
-- 连接时开了 ANSI_QUOTES（见 mysql.go），双引号就是标识符，
-- 而 SQLite 本来就这么解释——于是业务代码里的一句 SQL 两边都能跑。

CREATE TABLE IF NOT EXISTS boards (
    id          BIGINT       NOT NULL AUTO_INCREMENT PRIMARY KEY,
    "key"       VARCHAR(64)  NOT NULL UNIQUE,
    name        VARCHAR(191) NOT NULL,
    description VARCHAR(500) NOT NULL DEFAULT '',
    created_at  VARCHAR(40)  NOT NULL
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4;

CREATE TABLE IF NOT EXISTS members (
    id         BIGINT       NOT NULL AUTO_INCREMENT PRIMARY KEY,
    name       VARCHAR(191) NOT NULL UNIQUE,
    role       VARCHAR(16)  NOT NULL DEFAULT 'member' CHECK (role IN ('member', 'lead')),
    color      VARCHAR(32)  NOT NULL DEFAULT '#6b7280',
    active     TINYINT      NOT NULL DEFAULT 1,
    created_at VARCHAR(40)  NOT NULL
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4;

-- stages 是可自由配置的面试阶段，按看板隔离。
-- key 是稳定标识（改名不动它），label 才是展示名。
CREATE TABLE IF NOT EXISTS stages (
    id             BIGINT       NOT NULL AUTO_INCREMENT PRIMARY KEY,
    board_id       BIGINT       NOT NULL,
    "key"          VARCHAR(64)  NOT NULL,
    label          VARCHAR(191) NOT NULL,
    -- task: 任务阶段（在线测评这类），移进来挂一条只有 DDL 和链接的记录
    kind           VARCHAR(20)  NOT NULL DEFAULT 'normal'
                   CHECK (kind IN ('normal', 'interview', 'task', 'terminal_success', 'terminal_fail')),
    color          VARCHAR(32)  NOT NULL DEFAULT '#6b7280',
    requires_owner TINYINT      NOT NULL DEFAULT 0,
    -- skippable: 这一列允许被跨过去。中间隔着的列全部可跳过时，才准跨阶段流转。
    skippable      TINYINT      NOT NULL DEFAULT 0,
    position       DOUBLE       NOT NULL,
    created_at     VARCHAR(40)  NOT NULL,
    UNIQUE KEY uk_stages_board_key (board_id, "key"),
    KEY idx_stages_board (board_id, position),
    CONSTRAINT fk_stages_board FOREIGN KEY (board_id) REFERENCES boards (id) ON DELETE CASCADE
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4;

-- applications 是一条投递 / 一条面试流程，卡片即此表一行。
CREATE TABLE IF NOT EXISTS applications (
    id         BIGINT        NOT NULL AUTO_INCREMENT PRIMARY KEY,
    board_id   BIGINT        NOT NULL,
    seq        BIGINT        NOT NULL,
    company    VARCHAR(191)  NOT NULL,
    role       VARCHAR(191)  NOT NULL DEFAULT '',
    channel    VARCHAR(191)  NOT NULL DEFAULT '',
    notes      VARCHAR(4000) NOT NULL DEFAULT '',
    stage_key  VARCHAR(64)   NOT NULL,
    intent     VARCHAR(16)   NOT NULL DEFAULT 'normal' CHECK (intent IN ('low', 'normal', 'high')),
    owner_id   BIGINT        NULL,
    position   DOUBLE        NOT NULL DEFAULT 0,
    created_at VARCHAR(40)   NOT NULL,
    updated_at VARCHAR(40)   NOT NULL,
    UNIQUE KEY uk_applications_board_seq (board_id, seq),
    KEY idx_applications_board (board_id, stage_key, position),
    KEY idx_applications_owner (owner_id),
    CONSTRAINT fk_applications_board FOREIGN KEY (board_id) REFERENCES boards (id) ON DELETE CASCADE,
    CONSTRAINT fk_applications_owner FOREIGN KEY (owner_id) REFERENCES members (id) ON DELETE SET NULL,
    CONSTRAINT fk_applications_stage FOREIGN KEY (board_id, stage_key) REFERENCES stages (board_id, "key") ON UPDATE CASCADE
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4;

-- google_accounts 存 Google Calendar 的授权凭证。
-- 这是一个人自己用的看板，所以只有一行（CHECK 把 id 钉死在 1）。
CREATE TABLE IF NOT EXISTS google_accounts (
    id            BIGINT        NOT NULL PRIMARY KEY CHECK (id = 1),
    email         VARCHAR(191)  NOT NULL DEFAULT '',
    calendar_id   VARCHAR(191)  NOT NULL DEFAULT 'primary',
    access_token  VARCHAR(2048) NOT NULL DEFAULT '',
    -- refresh_token 是长期凭证，access_token 过期后靠它换新的
    refresh_token VARCHAR(2048) NOT NULL,
    expiry        VARCHAR(40)   NOT NULL DEFAULT '',
    connected_at  VARCHAR(40)   NOT NULL
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4;

-- interview_rounds 是每一轮面试 / 每一个任务的独立记录。
-- stage_label 存快照，阶段改名后历史仍然可读。
CREATE TABLE IF NOT EXISTS interview_rounds (
    id              BIGINT        NOT NULL AUTO_INCREMENT PRIMARY KEY,
    application_id  BIGINT        NOT NULL,
    stage_key       VARCHAR(64)   NOT NULL,
    stage_label     VARCHAR(191)  NOT NULL,
    -- kind 决定 scheduled_at 的含义，也决定表单和日程上怎么画：
    --   interview 面试：scheduled_at 是开始时间，duration_min 是时长
    --   task      测评：scheduled_at 是截止时间（DDL），duration_min 无意义，
    --                  meeting_url 存的是测评链接
    kind            VARCHAR(20)   NOT NULL DEFAULT 'interview'
                    CHECK (kind IN ('interview', 'task')),
    scheduled_at    VARCHAR(40)   NULL,
    duration_min    BIGINT        NOT NULL DEFAULT 60,
    mode            VARCHAR(16)   NOT NULL DEFAULT 'online' CHECK (mode IN ('online', 'onsite', 'phone')),
    meeting_url     VARCHAR(1000) NOT NULL DEFAULT '',
    meeting_place   VARCHAR(500)  NOT NULL DEFAULT '',
    interviewer     VARCHAR(191)  NOT NULL DEFAULT '',
    -- result: awaiting 是「面试已经结束、结果还没下来」的等待期，
    --         既不用再催时间，也还没有定论
    result          VARCHAR(16)   NOT NULL DEFAULT 'pending'
                    CONSTRAINT chk_rounds_result
                    CHECK (result IN ('pending', 'awaiting', 'passed', 'failed', 'cancelled')),
    notes           VARCHAR(4000) NOT NULL DEFAULT '',
    -- google_event_id: 这一轮在 Google 日历上对应的事件，空表示还没同步过
    google_event_id VARCHAR(191)  NOT NULL DEFAULT '',
    created_at      VARCHAR(40)   NOT NULL,
    updated_at      VARCHAR(40)   NOT NULL,
    KEY idx_rounds_app (application_id, scheduled_at),
    CONSTRAINT fk_rounds_app FOREIGN KEY (application_id) REFERENCES applications (id) ON DELETE CASCADE
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4;

CREATE TABLE IF NOT EXISTS application_events (
    id             BIGINT        NOT NULL AUTO_INCREMENT PRIMARY KEY,
    application_id BIGINT        NOT NULL,
    actor_id       BIGINT        NULL,
    type           VARCHAR(64)   NOT NULL,
    from_stage     VARCHAR(64)   NULL,
    to_stage       VARCHAR(64)   NULL,
    detail         VARCHAR(2000) NOT NULL DEFAULT '',
    created_at     VARCHAR(40)   NOT NULL,
    KEY idx_application_events_app (application_id, id),
    KEY idx_application_events_actor (actor_id),
    CONSTRAINT fk_events_app FOREIGN KEY (application_id) REFERENCES applications (id) ON DELETE CASCADE,
    CONSTRAINT fk_events_actor FOREIGN KEY (actor_id) REFERENCES members (id) ON DELETE SET NULL
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4;
