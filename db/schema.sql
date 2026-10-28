-- AutoBid Engine 存储层 DDL（幂等，可重复执行）。
-- 基线：ARTEX_CORE_BLUEPRINT.md 附录 B + 第 8.2 节 v2 Blueprint Patch ④⑤⑥。
-- 领域映射：assets = 招标文件静态要素；fact = 标书核验过程事实；finding = 经确认的投策/合规结论。

CREATE OR REPLACE FUNCTION set_updated_at() RETURNS trigger AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- ---------------------------------------------------------------------------
-- 任务推演空间（附录 B.2）
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS explorations (
    id          BIGSERIAL PRIMARY KEY,
    description TEXT,
    goal        TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'open'
                  CHECK (status IN ('open','achieved','failed')),
    round_no    BIGINT NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
DROP TRIGGER IF EXISTS trg_exp_upd ON explorations;
CREATE TRIGGER trg_exp_upd BEFORE UPDATE ON explorations
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------------------
-- 任务生命周期（§1.2 tasks.status；§2.7 Task 状态机的 paused/queued/deadline 字段）
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS tasks (
    id              BIGSERIAL PRIMARY KEY,
    name            TEXT NOT NULL,
    description     TEXT NOT NULL DEFAULT '',
    exploration_id  BIGINT NOT NULL UNIQUE REFERENCES explorations(id) ON DELETE RESTRICT,
    status          TEXT NOT NULL DEFAULT 'created'
                      CHECK (status IN ('created','running','paused','done','failed','timeout')),
    paused          BOOLEAN NOT NULL DEFAULT false,
    queued          BOOLEAN NOT NULL DEFAULT false,
    timeout_seconds INT NOT NULL DEFAULT 0 CHECK (timeout_seconds >= 0),
    first_run_at    TIMESTAMPTZ,
    deadline_at     TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Planner 心跳间隔（秒）；执行层下限/默认 600（§2.3）。
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS plan_heartbeat_seconds INT NOT NULL DEFAULT 600
    CHECK (plan_heartbeat_seconds >= 0);
DROP TRIGGER IF EXISTS trg_tasks_upd ON tasks;
CREATE TRIGGER trg_tasks_upd BEFORE UPDATE ON tasks
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- 显式直接来源任务；查询只读当前任务与直接来源，不递归（§1.1）。
CREATE TABLE IF NOT EXISTS task_relations (
    task_id        BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    source_task_id BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (task_id, source_task_id),
    CONSTRAINT ck_task_relation_noself CHECK (task_id <> source_task_id)
);
CREATE INDEX IF NOT EXISTS idx_task_relations_source ON task_relations(source_task_id);

-- ---------------------------------------------------------------------------
-- 全局静态实体：招标文件要素（替换附录 B.1 的网络资产类别体系，结构契约不变）
--   tender          招标项目/招标文件，自然键 tender_no
--   lot             标段/包，自然键 (tender_no, lot_no)
--   clause          条款（含 ★ 实质性条款、废标条款）
--   qualification   资质/资格要求
--   scoring_item    评分项
--   commercial_item 商务标的/报价清单项
-- lot_no='' 表示适用于全部标段；code 为文件内编号（如 "3.2.1"、"评分表-技术-05"）。
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS assets (
    id           BIGSERIAL PRIMARY KEY,
    type         TEXT NOT NULL,
    tender_no    TEXT NOT NULL,
    lot_no       TEXT NOT NULL DEFAULT '',
    code         TEXT NOT NULL DEFAULT '',
    title        TEXT NOT NULL DEFAULT '',
    content      TEXT NOT NULL DEFAULT '',
    category     TEXT NOT NULL DEFAULT '',
    is_mandatory BOOLEAN NOT NULL DEFAULT false,
    score_max    NUMERIC(10,4),
    amount       NUMERIC(20,4),
    currency     TEXT,
    quantity     NUMERIC(20,4),
    unit         TEXT,
    source_ref   JSONB NOT NULL DEFAULT '{}',
    extra        JSONB NOT NULL DEFAULT '{}',
    task_ids     BIGINT[] NOT NULL DEFAULT '{}',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT ck_asset_type CHECK (type IN (
        'tender','lot','clause','qualification','scoring_item','commercial_item'
    )),
    CONSTRAINT ck_asset_tender_no CHECK (tender_no <> ''),
    CONSTRAINT ck_asset_identity CHECK (
        (type = 'tender' AND lot_no = '' AND code = '') OR
        (type = 'lot'    AND lot_no <> '' AND code = '') OR
        (type IN ('clause','qualification','scoring_item','commercial_item') AND code <> '')
    ),
    CONSTRAINT ck_asset_score_max CHECK (score_max IS NULL OR score_max >= 0)
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_asset_tender  ON assets(tender_no)         WHERE type = 'tender';
CREATE UNIQUE INDEX IF NOT EXISTS uq_asset_lot     ON assets(tender_no, lot_no) WHERE type = 'lot';
CREATE UNIQUE INDEX IF NOT EXISTS uq_asset_element ON assets(type, tender_no, lot_no, code)
    WHERE type IN ('clause','qualification','scoring_item','commercial_item');
CREATE INDEX IF NOT EXISTS idx_asset_tender_type ON assets(tender_no, type);
CREATE INDEX IF NOT EXISTS idx_asset_task_ids    ON assets USING GIN(task_ids);
DROP TRIGGER IF EXISTS trg_assets_upd ON assets;
CREATE TRIGGER trg_assets_upd BEFORE UPDATE ON assets
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- 任务声明范围：覆盖率分母中的“声明范围匹配实体”。lot_no='' 表示整个招标项目。
CREATE TABLE IF NOT EXISTS task_scopes (
    task_id    BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    tender_no  TEXT NOT NULL CHECK (tender_no <> ''),
    lot_no     TEXT NOT NULL DEFAULT '',
    source     TEXT NOT NULL DEFAULT 'explicit' CHECK (source IN ('explicit','auto')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (task_id, tender_no, lot_no)
);
CREATE INDEX IF NOT EXISTS idx_task_scopes_tender ON task_scopes(tender_no, lot_no);

-- ---------------------------------------------------------------------------
-- 推演节点（附录 B.3 + B.4 + Patch ④ 租约列）
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS exploration_nodes (
    id               BIGSERIAL PRIMARY KEY,
    exploration_id   BIGINT NOT NULL REFERENCES explorations(id) ON DELETE CASCADE,
    kind             TEXT NOT NULL,
    payload          JSONB NOT NULL DEFAULT '{}',
    priority         INT  NOT NULL DEFAULT 0,
    state            TEXT NOT NULL DEFAULT 'open',
    origin           TEXT,
    owner            TEXT,
    blocked_reason   TEXT,
    delete_reason    TEXT,
    content_version  INT    NOT NULL DEFAULT 0,
    cold_since_round BIGINT,
    lease_until      TIMESTAMPTZ,
    lease_epoch      BIGINT NOT NULL DEFAULT 0,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at     TIMESTAMPTZ,
    CONSTRAINT ck_node_kind CHECK (kind IN ('begin','goal','intent','fact','finding','hint','digest')),
    CONSTRAINT ck_node_state CHECK (
        (kind='begin'   AND state IN ('open')) OR
        (kind='intent'  AND state IN ('open','running','paused','done','blocked','exhausted','stopped','deleted')) OR
        (kind='goal'    AND state IN ('open','met','abandoned')) OR
        (kind='fact'    AND state IN ('confirmed','dismissed','origin')) OR
        (kind='finding' AND state IN ('confirmed','dismissed')) OR
        (kind='hint'    AND state IN ('active','consumed')) OR
        (kind='digest'  AND state IN ('active','superseded'))
    )
);
-- 已部署旧库的增量补列（Patch ④ 原文）。
ALTER TABLE exploration_nodes
    ADD COLUMN IF NOT EXISTS lease_until TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS lease_epoch BIGINT NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS idx_expnodes_exp_kind ON exploration_nodes(exploration_id, kind, state);
-- Frontier：priority DESC, id ASC 取 open Intent（§2.5）。
CREATE INDEX IF NOT EXISTS idx_expnodes_frontier
    ON exploration_nodes(exploration_id, priority DESC, id)
    WHERE kind = 'intent' AND state = 'open';
-- Patch ④：过期 running 意图回收。
CREATE INDEX IF NOT EXISTS idx_expnodes_running_lease
    ON exploration_nodes(exploration_id, lease_until)
    WHERE kind = 'intent' AND state = 'running';
DROP TRIGGER IF EXISTS trg_expnodes_upd ON exploration_nodes;
CREATE TRIGGER trg_expnodes_upd BEFORE UPDATE ON exploration_nodes
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------------------
-- 推演边（附录 B.5）。kind 矩阵与跨空间来源例外由 Go 层 linkTx 在同一事务内校验。
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS exploration_edges (
    exploration_id BIGINT NOT NULL REFERENCES explorations(id) ON DELETE CASCADE,
    src_id         BIGINT NOT NULL REFERENCES exploration_nodes(id) ON DELETE CASCADE,
    dst_id         BIGINT NOT NULL REFERENCES exploration_nodes(id) ON DELETE CASCADE,
    rel            TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (exploration_id, src_id, rel, dst_id),
    CONSTRAINT ck_edge_noself CHECK (src_id <> dst_id),
    CONSTRAINT ck_edge_rel CHECK (rel IN ('spawns','derived_from','yields','proves','covers'))
);
CREATE INDEX IF NOT EXISTS idx_expedges_src ON exploration_edges(src_id, rel);
CREATE INDEX IF NOT EXISTS idx_expedges_dst ON exploration_edges(dst_id, rel);

-- 跨图锚点（附录 B.6）。
CREATE TABLE IF NOT EXISTS exploration_anchors (
    node_id   BIGINT NOT NULL REFERENCES exploration_nodes(id) ON DELETE CASCADE,
    asset_id  BIGINT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    PRIMARY KEY (node_id, asset_id)
);
CREATE INDEX IF NOT EXISTS idx_anchor_asset ON exploration_anchors(asset_id);

-- 任务与实体的来源记录（谁引入了该实体），区别于锚点（哪个节点涉及该实体）。
CREATE TABLE IF NOT EXISTS task_asset_links (
    task_id        BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    asset_id       BIGINT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    source_node_id BIGINT REFERENCES exploration_nodes(id) ON DELETE SET NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (task_id, asset_id)
);
CREATE INDEX IF NOT EXISTS idx_task_asset_links_asset ON task_asset_links(asset_id);

-- ---------------------------------------------------------------------------
-- Patch ⑥：静态实体显式有向边表（例：tender -contains-> lot，lot -requires-> qualification）
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS entity_edges (
    id             BIGSERIAL PRIMARY KEY,
    src_asset_id   BIGINT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    dst_asset_id   BIGINT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    rel            TEXT   NOT NULL,
    task_ids       BIGINT[] NOT NULL DEFAULT '{}',
    source_node_id BIGINT REFERENCES exploration_nodes(id) ON DELETE SET NULL,
    weight         DOUBLE PRECISION NOT NULL DEFAULT 1.0,
    props          JSONB  NOT NULL DEFAULT '{}',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT ck_entity_edge_noself CHECK (src_asset_id <> dst_asset_id),
    CONSTRAINT ck_entity_edge_rel    CHECK (rel <> ''),
    CONSTRAINT uq_entity_edge        UNIQUE (src_asset_id, rel, dst_asset_id)
);
CREATE INDEX IF NOT EXISTS idx_entity_edges_src      ON entity_edges(src_asset_id, rel);
CREATE INDEX IF NOT EXISTS idx_entity_edges_dst      ON entity_edges(dst_asset_id, rel);
CREATE INDEX IF NOT EXISTS idx_entity_edges_task_ids ON entity_edges USING GIN(task_ids);

-- ---------------------------------------------------------------------------
-- 产出物记录：finding 节点之外的独立结论记录与证据关联（§1.5 report_finding）。
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS findings (
    id             BIGSERIAL PRIMARY KEY,
    task_id        BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    exploration_id BIGINT NOT NULL REFERENCES explorations(id) ON DELETE CASCADE,
    node_id        BIGINT NOT NULL UNIQUE REFERENCES exploration_nodes(id) ON DELETE CASCADE,
    intent_id      BIGINT REFERENCES exploration_nodes(id) ON DELETE SET NULL,
    category       TEXT NOT NULL
                     CHECK (category IN ('compliance','risk','scoring','pricing','strategy')),
    severity       TEXT NOT NULL DEFAULT 'info'
                     CHECK (severity IN ('info','low','medium','high','critical')),
    title          TEXT NOT NULL CHECK (title <> ''),
    summary        TEXT NOT NULL DEFAULT '',
    payload        JSONB NOT NULL DEFAULT '{}',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_findings_task ON findings(task_id, category, severity);
DROP TRIGGER IF EXISTS trg_findings_upd ON findings;
CREATE TRIGGER trg_findings_upd BEFORE UPDATE ON findings
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE IF NOT EXISTS finding_evidence (
    finding_id BIGINT NOT NULL REFERENCES findings(id) ON DELETE CASCADE,
    node_id    BIGINT NOT NULL REFERENCES exploration_nodes(id) ON DELETE CASCADE,
    PRIMARY KEY (finding_id, node_id)
);
CREATE INDEX IF NOT EXISTS idx_finding_evidence_node ON finding_evidence(node_id);

-- ---------------------------------------------------------------------------
-- Agent 会话 transcript（Worker 以 exp<expID>-worker-i<intentID> 稳定命名，§2.6）。
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS transcripts (
    session_id TEXT PRIMARY KEY,
    messages   JSONB NOT NULL DEFAULT '[]',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
DROP TRIGGER IF EXISTS trg_transcripts_upd ON transcripts;
CREATE TRIGGER trg_transcripts_upd BEFORE UPDATE ON transcripts
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------------------
-- LLM 配置（§5.4–5.7）。凭据不入库：api_key_env 只记录读取密钥的环境变量名。
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS llm_profiles (
    id               BIGSERIAL PRIMARY KEY,
    name             TEXT NOT NULL UNIQUE,
    format           TEXT NOT NULL DEFAULT 'anthropic',
    base_url         TEXT NOT NULL DEFAULT '',
    api_key_env      TEXT NOT NULL DEFAULT 'ANTHROPIC_API_KEY',
    model            TEXT NOT NULL,
    max_tokens       INT  NOT NULL DEFAULT 0 CHECK (max_tokens >= 0),
    effort           TEXT NOT NULL DEFAULT 'high'
                       CHECK (effort IN ('','low','medium','high','xhigh','max')),
    thinking_display TEXT NOT NULL DEFAULT '' CHECK (thinking_display IN ('','summarized','omitted')),
    priority         INT  NOT NULL DEFAULT 0,
    rank             INT  NOT NULL DEFAULT 0,
    window_tokens    INT  NOT NULL DEFAULT 0 CHECK (window_tokens >= 0),
    enabled          BOOLEAN NOT NULL DEFAULT true,
    in_global_pool   BOOLEAN NOT NULL DEFAULT true,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
DROP TRIGGER IF EXISTS trg_llm_profiles_upd ON llm_profiles;
CREATE TRIGGER trg_llm_profiles_upd BEFORE UPDATE ON llm_profiles
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- 角色绑定：优先级高于任务显式链与全局池。
CREATE TABLE IF NOT EXISTS agent_llm_bindings (
    agent_key  TEXT PRIMARY KEY CHECK (agent_key IN ('planner','worker','judge')),
    profile_id BIGINT NOT NULL REFERENCES llm_profiles(id) ON DELETE CASCADE,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 任务显式 Profile 链：共享游标 + revision 防陈旧请求覆盖新链。
ALTER TABLE tasks
    ADD COLUMN IF NOT EXISTS active_llm_profile_id BIGINT REFERENCES llm_profiles(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS llm_chain_revision    BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS llm_failover_state    TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS llm_failover_reason   TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS task_llm_profiles (
    task_id      BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    position     INT    NOT NULL CHECK (position >= 0),
    profile_id   BIGINT NOT NULL REFERENCES llm_profiles(id) ON DELETE CASCADE,
    status       TEXT   NOT NULL DEFAULT 'ready' CHECK (status IN ('ready','quota_exhausted')),
    exhausted_at TIMESTAMPTZ,
    PRIMARY KEY (task_id, position),
    UNIQUE (task_id, profile_id)
);

-- 独立于正文录制的 token 账本：每次真实 Provider 调用一行（含失败与取消）。
CREATE TABLE IF NOT EXISTS llm_usage (
    id             BIGSERIAL PRIMARY KEY,
    task_id        BIGINT REFERENCES tasks(id) ON DELETE SET NULL,
    exploration_id BIGINT,
    session_id     TEXT NOT NULL DEFAULT '',
    worker         TEXT NOT NULL DEFAULT '',
    model          TEXT NOT NULL DEFAULT '',
    profile_id     BIGINT,
    profile_name   TEXT NOT NULL DEFAULT '',
    effort         TEXT NOT NULL DEFAULT '',
    streaming      BOOLEAN NOT NULL DEFAULT false,
    latency_ms     INT NOT NULL DEFAULT 0,
    input_tokens   INT NOT NULL DEFAULT 0,
    output_tokens  INT NOT NULL DEFAULT 0,
    cache_read     INT NOT NULL DEFAULT 0,
    cache_write    INT NOT NULL DEFAULT 0,
    status         TEXT NOT NULL CHECK (status IN ('ok','error','cancelled')),
    error          TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_llm_usage_task ON llm_usage(task_id, created_at);

-- llmpool 熔断状态持久化，重启后恢复仍有效的熔断。
CREATE TABLE IF NOT EXISTS llm_profile_health (
    profile_id BIGINT PRIMARY KEY REFERENCES llm_profiles(id) ON DELETE CASCADE,
    fails      INT NOT NULL DEFAULT 0,
    trips      INT NOT NULL DEFAULT 0,
    open_until TIMESTAMPTZ,
    last_error TEXT NOT NULL DEFAULT '',
    last_at    TIMESTAMPTZ
);

-- Profile 网络与格式扩展：OpenAI 兼容格式、出站代理、会话亲和请求头。
ALTER TABLE llm_profiles DROP CONSTRAINT IF EXISTS llm_profiles_format_check;
ALTER TABLE llm_profiles ADD CONSTRAINT llm_profiles_format_check CHECK (format IN ('anthropic','openai'));
ALTER TABLE llm_profiles
    ADD COLUMN IF NOT EXISTS proxy              TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS session_header_key TEXT NOT NULL DEFAULT '';

-- 可关闭的原始报文录制（只录请求/响应正文，不录请求头，避免泄露凭据）。
CREATE TABLE IF NOT EXISTS llm_records (
    id             BIGSERIAL PRIMARY KEY,
    task_id        BIGINT REFERENCES tasks(id) ON DELETE SET NULL,
    exploration_id BIGINT,
    session_id     TEXT NOT NULL DEFAULT '',
    worker         TEXT NOT NULL DEFAULT '',
    profile_id     BIGINT,
    profile_name   TEXT NOT NULL DEFAULT '',
    method         TEXT NOT NULL DEFAULT '',
    url            TEXT NOT NULL DEFAULT '',
    status_code    INT  NOT NULL DEFAULT 0,
    latency_ms     INT  NOT NULL DEFAULT 0,
    request_body   TEXT NOT NULL DEFAULT '',
    response_body  TEXT NOT NULL DEFAULT '',
    truncated      BOOLEAN NOT NULL DEFAULT false,
    error          TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_llm_records_task ON llm_records(task_id, created_at);

-- ---------------------------------------------------------------------------
-- 拦截、审批挂起与 Guard 控制面（§6）。
-- ---------------------------------------------------------------------------
-- 规则：priority DESC, id ASC 取第一条命中；tool_filter 为空表示作用于全部启用拦截的工具（本系统扩展）。
CREATE TABLE IF NOT EXISTS intercept_rules (
    id              BIGSERIAL PRIMARY KEY,
    name            TEXT NOT NULL DEFAULT '',
    tool_filter     TEXT NOT NULL DEFAULT '',
    match_target    TEXT NOT NULL DEFAULT 'tool_name' CHECK (match_target IN ('tool_name', 'tool_input')),
    match_type      TEXT NOT NULL DEFAULT 'string' CHECK (match_type IN ('string', 'regex')),
    pattern         TEXT NOT NULL,
    action          TEXT NOT NULL CHECK (action IN ('allow', 'deny', 'ask')),
    message         TEXT NOT NULL DEFAULT '',
    priority        INT  NOT NULL DEFAULT 0,
    enabled         BOOLEAN NOT NULL DEFAULT true,
    timeout_enabled BOOLEAN NOT NULL DEFAULT false,
    timeout_seconds INT  NOT NULL DEFAULT 0,
    timeout_action  TEXT NOT NULL DEFAULT 'deny', -- 蓝图 §6.1：无 CHECK
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
DROP TRIGGER IF EXISTS trg_intercept_rules_upd ON intercept_rules;
CREATE TRIGGER trg_intercept_rules_upd BEFORE UPDATE ON intercept_rules
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- 审批挂起：DB 状态为准（§6.3）；只决定一次由 WHERE status='pending' 条件 UPDATE 保证。
-- waiter/waiter_until 是等待方心跳：等待方失联（进程重启）的 pending 被回收为 denied（§8.2 缺口的最小兜底）。
CREATE TABLE IF NOT EXISTS intercept_pending (
    id               BIGSERIAL PRIMARY KEY,
    rule_id          BIGINT REFERENCES intercept_rules(id) ON DELETE SET NULL,
    task_id          BIGINT REFERENCES tasks(id) ON DELETE CASCADE,
    intent_id        BIGINT,
    agent_name       TEXT NOT NULL DEFAULT '',
    tool_name        TEXT NOT NULL,
    tool_input       TEXT NOT NULL DEFAULT '',
    message          TEXT NOT NULL DEFAULT '',
    status           TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'allowed', 'denied', 'timeout')),
    effective_action TEXT NOT NULL DEFAULT '',
    reason           TEXT NOT NULL DEFAULT '',
    timeout_action   TEXT NOT NULL DEFAULT 'deny',
    audit            JSONB NOT NULL DEFAULT '{}'::jsonb,
    waiter           TEXT NOT NULL DEFAULT '',
    waiter_until     TIMESTAMPTZ,
    exec_outcome     TEXT NOT NULL DEFAULT '',
    exec_detail      TEXT NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_at       TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_intercept_pending_open ON intercept_pending(waiter_until) WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS idx_intercept_pending_task ON intercept_pending(task_id, created_at);

-- 已决审计：规则/Judge/默认放行的每一次判定（ask 的最终结果也落一行），Guard 内存审计不作为审批证明。
CREATE TABLE IF NOT EXISTS intercept_audit (
    id               BIGSERIAL PRIMARY KEY,
    task_id          BIGINT REFERENCES tasks(id) ON DELETE SET NULL,
    intent_id        BIGINT,
    agent_name       TEXT NOT NULL DEFAULT '',
    tool_name        TEXT NOT NULL,
    input_digest     TEXT NOT NULL DEFAULT '',
    source           TEXT NOT NULL CHECK (source IN ('rule', 'judge', 'default', 'ask')),
    rule_id          BIGINT,
    profile_id       BIGINT,
    action           TEXT NOT NULL,
    effective_action TEXT NOT NULL,
    pending_id       BIGINT,
    reason           TEXT NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_intercept_audit_task ON intercept_audit(task_id, created_at);

-- 控制面配置（单行）：enabled_tools 为 NULL 时用内置默认集合；judge 为 JudgeConfig JSON。
CREATE TABLE IF NOT EXISTS intercept_config (
    id            INT PRIMARY KEY CHECK (id = 1),
    enabled_tools TEXT[],
    judge         JSONB NOT NULL DEFAULT '{}'::jsonb,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- 冷节点与 Digest 折叠（§1.6、附录 B.4）。active_round 记录节点最近一次状态/内容写入时的规划轮次，
-- 由触发器维护：任何 state / payload / content_version 写入都会“回温”（清空 cold_since_round）。
-- ---------------------------------------------------------------------------
ALTER TABLE exploration_nodes ADD COLUMN IF NOT EXISTS active_round BIGINT NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS idx_expnodes_cold ON exploration_nodes(exploration_id, cold_since_round)
    WHERE cold_since_round IS NOT NULL;

CREATE OR REPLACE FUNCTION expnode_touch_round() RETURNS trigger AS $$
BEGIN
    NEW.active_round := COALESCE((SELECT round_no FROM explorations WHERE id = NEW.exploration_id), 0);
    NEW.cold_since_round := NULL;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_expnodes_touch ON exploration_nodes;
CREATE TRIGGER trg_expnodes_touch BEFORE INSERT OR UPDATE OF state, payload, content_version ON exploration_nodes
    FOR EACH ROW EXECUTE FUNCTION expnode_touch_round();
