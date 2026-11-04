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
-- D3 句子级落证门禁是否强制启用：新任务默认 true；加列时已有任务填 false，保持原有"有故事板或核验记录才启用"的行为。
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS grounded_writing_required BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE tasks ALTER COLUMN grounded_writing_required SET DEFAULT true;
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
-- LLM 配置（§5.4–5.7）。兼容环境变量凭据；UI Key 由后续幂等 DDL 增补的加密字段保存。
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
    agent_key  TEXT PRIMARY KEY CHECK (agent_key IN ('planner','worker','judge','goals','watchdog')),
    profile_id BIGINT NOT NULL REFERENCES llm_profiles(id) ON DELETE CASCADE,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
ALTER TABLE agent_llm_bindings DROP CONSTRAINT IF EXISTS agent_llm_bindings_agent_key_check;
ALTER TABLE agent_llm_bindings ADD CONSTRAINT agent_llm_bindings_agent_key_check CHECK (agent_key IN ('planner','worker','judge','goals','watchdog'));

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
ALTER TABLE llm_profiles ADD CONSTRAINT llm_profiles_format_check CHECK (format IN ('anthropic','openai','openai-responses'));
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

-- ---------------------------------------------------------------------------
-- Phase 2：招标原文与企业资料库。
--   tender_documents  任务上传的招标文件原文；content_hash 保证同一任务重复上传幂等。
--   document_chunks   按章节/条款切分的段落；seq 为文档内顺序（read_chunk 相邻窗口），
--                     is_mandatory 标记 ★/*/废标/否决等强制条款。
--   company_credentials 本企业资质/业绩/人员库（全局，不随任务删除）；code 非空时 (category, code) 唯一。
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS tender_documents (
    id           BIGSERIAL PRIMARY KEY,
    task_id      BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    tender_no    TEXT NOT NULL CHECK (tender_no <> ''),
    lot_no       TEXT NOT NULL DEFAULT '',
    title        TEXT NOT NULL DEFAULT '',
    raw_text     TEXT NOT NULL DEFAULT '',
    content_hash TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS document_chunks (
    id             BIGSERIAL PRIMARY KEY,
    doc_id         BIGINT NOT NULL REFERENCES tender_documents(id) ON DELETE CASCADE,
    task_id        BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    seq            INT  NOT NULL CHECK (seq >= 0),
    level          INT  NOT NULL DEFAULT 0,
    section_code   TEXT NOT NULL DEFAULT '',
    section_title  TEXT NOT NULL DEFAULT '',
    page_no        INT  NOT NULL DEFAULT 1,
    content        TEXT NOT NULL DEFAULT '',
    token_estimate INT  NOT NULL DEFAULT 0,
    is_mandatory   BOOLEAN NOT NULL DEFAULT false,
    UNIQUE (doc_id, seq)
);
CREATE INDEX IF NOT EXISTS idx_document_chunks_task ON document_chunks(task_id, doc_id, seq);

CREATE TABLE IF NOT EXISTS company_credentials (
    id          BIGSERIAL PRIMARY KEY,
    category    TEXT NOT NULL CHECK (category <> ''),
    code        TEXT NOT NULL DEFAULT '',
    title       TEXT NOT NULL CHECK (title <> ''),
    attributes  JSONB NOT NULL DEFAULT '{}',
    valid_until DATE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_company_credentials_category ON company_credentials(category);
DROP TRIGGER IF EXISTS trg_company_credentials_upd ON company_credentials;
CREATE TRIGGER trg_company_credentials_upd BEFORE UPDATE ON company_credentials
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------------------
-- Phase 4：多格式文档解析溯源（域 A 扩展）与企业方案知识库（域 D）。
--   tender_documents  补充来源文件元数据：file_name / mime_type / page_count / parse_meta（解析器、OCR、告警），
--                     content_hash 对二进制文件为原始字节 SHA-256；幂等唯一键纳入 tenant_id。
--   document_chunks   page_start~page_end 为段落跨越的页码范围（page_no 保留为起始页，兼容旧接口），
--                     tables_json 为段落内还原的结构化表格，bbox_meta 为版面坐标/段落序号溯源。
-- ---------------------------------------------------------------------------
-- F1：先移除旧的整行检索失效触发器，避免下面的 page_start 回填按整行触发并误伤检索证据；
-- 新触发器在本节末尾按内容列重建（元数据列更新不得使 evidence 失效）。
DROP TRIGGER IF EXISTS trg_tender_retrieval_stale ON tender_documents;
DROP TRIGGER IF EXISTS trg_chunks_retrieval_stale ON document_chunks;
ALTER TABLE tender_documents
    ADD COLUMN IF NOT EXISTS tenant_id  TEXT NOT NULL DEFAULT 'default',
    ADD COLUMN IF NOT EXISTS file_name  TEXT,
    ADD COLUMN IF NOT EXISTS mime_type  TEXT,
    ADD COLUMN IF NOT EXISTS page_count INT,
    ADD COLUMN IF NOT EXISTS parse_meta JSONB,
    ADD COLUMN IF NOT EXISTS created_by TEXT;
-- D1：招标包内的文件角色与发布时间。后发布的更正 / 澄清优先于原招标文件（例如投标截止时间）。
ALTER TABLE tender_documents
    ADD COLUMN IF NOT EXISTS doc_role     TEXT NOT NULL DEFAULT 'tender'
        CHECK (doc_role IN ('tender','correction','clarification','qa','attachment')),
    ADD COLUMN IF NOT EXISTS published_at TIMESTAMPTZ;
DROP INDEX IF EXISTS uq_tender_documents_content;
CREATE UNIQUE INDEX IF NOT EXISTS uq_tender_documents_tenant_content
    ON tender_documents(tenant_id, task_id, tender_no, lot_no, content_hash);

ALTER TABLE document_chunks
    ADD COLUMN IF NOT EXISTS page_start  INT,
    ADD COLUMN IF NOT EXISTS page_end    INT,
    ADD COLUMN IF NOT EXISTS tables_json JSONB,
    ADD COLUMN IF NOT EXISTS bbox_meta   JSONB;
UPDATE document_chunks SET page_start = page_no, page_end = page_no
    WHERE page_start IS NULL AND COALESCE(bbox_meta->>'page_is_physical', 'true') <> 'false';

-- ---------------------------------------------------------------------------
-- F1：任务原件登记与正式解析修订快照（域 A 溯源层）。
--   task_file_objects     内容寻址的物理对象按租户共享，行按任务登记；relative_path 只能由服务端生成。
--   tender_parse_revisions 保存被替换前的正文与分块全集；写入后不可修改。当前正文仍在 tender_documents /
--                         document_chunks，revision_no 为被替换前的 parse_revision。
--   招标原件不得写入企业证据表（D2 company_evidence_originals / evidence.Store）。
-- ---------------------------------------------------------------------------
ALTER TABLE tender_documents
    ADD COLUMN IF NOT EXISTS original_file_id BIGINT,
    ADD COLUMN IF NOT EXISTS parse_revision   INT NOT NULL DEFAULT 0 CHECK (parse_revision >= 0);
CREATE UNIQUE INDEX IF NOT EXISTS uq_tender_documents_tenant_task_id
    ON tender_documents(tenant_id, task_id, id);

CREATE TABLE IF NOT EXISTS task_file_objects (
    id             BIGSERIAL PRIMARY KEY,
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    task_id        BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    kind           TEXT NOT NULL CHECK (kind IN ('original','script','derived','text_snapshot')),
    sha256         TEXT NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    byte_size      BIGINT NOT NULL CHECK (byte_size >= 0),
    relative_path  TEXT NOT NULL CHECK (relative_path <> ''),
    original_name  TEXT NOT NULL DEFAULT '',
    media_type     TEXT NOT NULL DEFAULT '',
    archive_path   TEXT NOT NULL DEFAULT '',
    source_job_id  BIGINT,
    parent_file_id BIGINT,
    availability   TEXT NOT NULL DEFAULT 'available' CHECK (availability IN ('available','missing','quarantined')),
    created_by     TEXT NOT NULL DEFAULT '',
    updated_by     TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_task_file_objects_task_id UNIQUE (tenant_id, task_id, id),
    CONSTRAINT fk_task_file_objects_parent FOREIGN KEY (tenant_id, task_id, parent_file_id)
        REFERENCES task_file_objects(tenant_id, task_id, id) ON DELETE CASCADE
);
ALTER TABLE task_file_objects
    ADD COLUMN IF NOT EXISTS format         TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS process_status TEXT NOT NULL DEFAULT 'listed'
        CHECK (process_status IN ('listed','processing','parsed','failed','unsupported','adopted'));
CREATE UNIQUE INDEX IF NOT EXISTS uq_task_file_objects_bytes
    ON task_file_objects(tenant_id, task_id, sha256, archive_path);
CREATE INDEX IF NOT EXISTS idx_task_file_objects_task ON task_file_objects(tenant_id, task_id, kind, id);
CREATE INDEX IF NOT EXISTS idx_task_file_objects_path ON task_file_objects(tenant_id, relative_path);
DROP TRIGGER IF EXISTS trg_task_file_objects_upd ON task_file_objects;
CREATE TRIGGER trg_task_file_objects_upd BEFORE UPDATE ON task_file_objects
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'fk_tender_documents_original_file') THEN
        ALTER TABLE tender_documents
            ADD CONSTRAINT fk_tender_documents_original_file
            FOREIGN KEY (tenant_id, task_id, original_file_id)
            REFERENCES task_file_objects(tenant_id, task_id, id)
            ON DELETE SET NULL (original_file_id);
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS tender_parse_revisions (
    id              BIGSERIAL PRIMARY KEY,
    tenant_id       TEXT NOT NULL DEFAULT 'default',
    task_id         BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    document_id     BIGINT NOT NULL,
    revision_no     INT NOT NULL CHECK (revision_no >= 0),
    original_file_id BIGINT,
    source_job_id   BIGINT,
    parser_version  TEXT NOT NULL DEFAULT '',
    raw_text        TEXT NOT NULL DEFAULT '',
    chunks_snapshot JSONB NOT NULL DEFAULT '[]',
    parse_meta      JSONB NOT NULL DEFAULT '{}',
    adopted_by      TEXT NOT NULL DEFAULT '',
    adopted_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by      TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_tender_parse_revisions_no UNIQUE (tenant_id, document_id, revision_no),
    FOREIGN KEY (tenant_id, task_id, document_id)
        REFERENCES tender_documents(tenant_id, task_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id, task_id, original_file_id)
        REFERENCES task_file_objects(tenant_id, task_id, id) ON DELETE SET NULL (original_file_id)
);
CREATE INDEX IF NOT EXISTS idx_tender_parse_revisions_doc
    ON tender_parse_revisions(tenant_id, task_id, document_id, revision_no DESC);

CREATE OR REPLACE FUNCTION tender_parse_revision_immutable() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'tender parse revision % is immutable', OLD.id USING ERRCODE = 'check_violation';
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_tender_parse_revisions_immutable ON tender_parse_revisions;
CREATE TRIGGER trg_tender_parse_revisions_immutable BEFORE UPDATE ON tender_parse_revisions
    FOR EACH ROW EXECUTE FUNCTION tender_parse_revision_immutable();

-- F3a fixed-parser records (domain B). No script runner or execution queue.
CREATE TABLE IF NOT EXISTS file_processing_records (
    id BIGSERIAL PRIMARY KEY,
    tenant_id TEXT NOT NULL DEFAULT 'default',
    task_id BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    file_id BIGINT NOT NULL,
    operation TEXT NOT NULL CHECK (operation IN ('import','reparse')),
    request_key TEXT NOT NULL CHECK (length(request_key) BETWEEN 1 AND 128),
    fingerprint TEXT NOT NULL,
    source_hash TEXT NOT NULL,
    target_document_id BIGINT,
    expected_revision INT NOT NULL DEFAULT 0,
    status TEXT NOT NULL CHECK (status IN ('ready','adopted','failed')),
    payload JSONB NOT NULL DEFAULT '{}',
    preview JSONB NOT NULL DEFAULT '{}',
    result JSONB NOT NULL DEFAULT '{}',
    error TEXT NOT NULL DEFAULT '',
    created_by TEXT NOT NULL DEFAULT '',
    updated_by TEXT NOT NULL DEFAULT '',
    approved_by TEXT NOT NULL DEFAULT '',
    review_note TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    adopted_at TIMESTAMPTZ,
    UNIQUE (tenant_id, task_id, request_key),
    FOREIGN KEY (tenant_id, task_id, file_id)
        REFERENCES task_file_objects(tenant_id, task_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id, task_id, target_document_id)
        REFERENCES tender_documents(tenant_id, task_id, id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_file_processing_records_task ON file_processing_records(tenant_id, task_id, id DESC);
CREATE UNIQUE INDEX IF NOT EXISTS uq_file_processing_records_task_id ON file_processing_records(tenant_id,task_id,id);

-- F3a+: fixed processing only. No user scripts or sandbox executor.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS file_gate_version BIGINT NOT NULL DEFAULT 0;
CREATE TABLE IF NOT EXISTS task_materials (
    id BIGSERIAL PRIMARY KEY,
    tenant_id TEXT NOT NULL DEFAULT 'default',
    task_id BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    name TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
    required BOOLEAN NOT NULL DEFAULT true,
    file_id BIGINT,
    version INT NOT NULL DEFAULT 1,
    note TEXT NOT NULL DEFAULT '',
    created_by TEXT NOT NULL DEFAULT '', updated_by TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(), updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, task_id, name),
    UNIQUE (tenant_id, task_id, id),
    FOREIGN KEY (tenant_id, task_id, file_id) REFERENCES task_file_objects(tenant_id, task_id, id)
        ON DELETE SET NULL (file_id)
);
CREATE TABLE IF NOT EXISTS task_material_events (
    id BIGSERIAL PRIMARY KEY,
    tenant_id TEXT NOT NULL DEFAULT 'default',
    task_id BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    material_id BIGINT NOT NULL,
    snapshot JSONB NOT NULL,
    created_by TEXT NOT NULL DEFAULT '', created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (tenant_id, task_id, material_id) REFERENCES task_materials(tenant_id, task_id, id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS fixed_file_jobs (
    id BIGSERIAL PRIMARY KEY,
    tenant_id TEXT NOT NULL DEFAULT 'default',
    task_id BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    file_id BIGINT NOT NULL,
    source_hash TEXT NOT NULL,
    request_key TEXT NOT NULL CHECK (length(request_key) BETWEEN 1 AND 128),
    fingerprint TEXT NOT NULL,
    input JSONB NOT NULL,
    status TEXT NOT NULL DEFAULT 'queued' CHECK (status IN ('queued','running','succeeded','failed','cancelled')),
    attempts INT NOT NULL DEFAULT 0,
    max_attempts INT NOT NULL DEFAULT 3 CHECK (max_attempts BETWEEN 1 AND 3),
    deadline_at TIMESTAMPTZ NOT NULL DEFAULT (now() + interval '15 minutes'),
    lease_token BIGINT NOT NULL DEFAULT 0,
    lease_until TIMESTAMPTZ, lease_owner TEXT NOT NULL DEFAULT '',
    record_id BIGINT REFERENCES file_processing_records(id) ON DELETE SET NULL,
    error TEXT NOT NULL DEFAULT '',
    created_by TEXT NOT NULL DEFAULT '', updated_by TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(), updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, task_id, request_key),
    FOREIGN KEY (tenant_id, task_id, file_id) REFERENCES task_file_objects(tenant_id, task_id, id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_fixed_file_jobs_queue ON fixed_file_jobs(status, id);
CREATE INDEX IF NOT EXISTS idx_fixed_file_jobs_task ON fixed_file_jobs(tenant_id, task_id, id DESC);
CREATE UNIQUE INDEX IF NOT EXISTS uq_fixed_file_jobs_task_id ON fixed_file_jobs(tenant_id,task_id,id);
DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname='fk_fixed_file_job_record' AND conrelid='fixed_file_jobs'::regclass) THEN
        ALTER TABLE fixed_file_jobs ADD CONSTRAINT fk_fixed_file_job_record FOREIGN KEY (tenant_id,task_id,record_id)
            REFERENCES file_processing_records(tenant_id,task_id,id) ON DELETE SET NULL (record_id);
    END IF;
END $$;
CREATE TABLE IF NOT EXISTS fixed_file_job_events (
    id BIGSERIAL PRIMARY KEY,
    job_id BIGINT NOT NULL REFERENCES fixed_file_jobs(id) ON DELETE CASCADE,
    tenant_id TEXT NOT NULL DEFAULT 'default', task_id BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    status TEXT NOT NULL, attempt INT NOT NULL, lease_token BIGINT NOT NULL,
    error TEXT NOT NULL DEFAULT '', created_by TEXT NOT NULL DEFAULT '', created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (tenant_id,task_id,job_id) REFERENCES fixed_file_jobs(tenant_id,task_id,id) ON DELETE CASCADE
);
CREATE OR REPLACE FUNCTION fixed_file_job_audit() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'UPDATE' AND (NEW.input, NEW.fingerprint, NEW.source_hash, NEW.file_id, NEW.request_key, NEW.tenant_id, NEW.task_id, NEW.deadline_at, NEW.max_attempts)
        IS DISTINCT FROM (OLD.input, OLD.fingerprint, OLD.source_hash, OLD.file_id, OLD.request_key, OLD.tenant_id, OLD.task_id, OLD.deadline_at, OLD.max_attempts) THEN
        RAISE EXCEPTION 'fixed processing input is immutable' USING ERRCODE = 'check_violation';
    END IF;
    IF TG_OP = 'INSERT' OR (NEW.status, NEW.lease_token) IS DISTINCT FROM (OLD.status, OLD.lease_token) THEN
        INSERT INTO fixed_file_job_events(job_id, tenant_id, task_id, status, attempt, lease_token, error, created_by)
        VALUES (NEW.id, NEW.tenant_id, NEW.task_id, NEW.status, NEW.attempts, NEW.lease_token, NEW.error, NEW.updated_by);
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_fixed_file_job_audit ON fixed_file_jobs;
CREATE TRIGGER trg_fixed_file_job_audit AFTER INSERT OR UPDATE ON fixed_file_jobs FOR EACH ROW EXECUTE FUNCTION fixed_file_job_audit();

-- 企业方案与产品知识库（域 D，严禁混入 assets）。
--   category：historical_bid 历史标书 / whitepaper 技术白皮书 / product_spec 产品规格书 / standard_solution 标准技术方案。
--   content_hash 在租户内幂等去重；extra 保存产品名、厂商、项目年份等可选元数据。
CREATE TABLE IF NOT EXISTS knowledge_documents (
    id           BIGSERIAL PRIMARY KEY,
    tenant_id    TEXT NOT NULL DEFAULT 'default',
    category     TEXT NOT NULL
                   CHECK (category IN ('historical_bid','whitepaper','product_spec','standard_solution')),
    title        TEXT NOT NULL CHECK (title <> ''),
    tags         TEXT[] NOT NULL DEFAULT '{}',
    file_name    TEXT NOT NULL DEFAULT '',
    mime_type    TEXT NOT NULL DEFAULT '',
    page_count   INT  NOT NULL DEFAULT 0,
    content_hash TEXT NOT NULL CHECK (content_hash <> ''),
    raw_text     TEXT NOT NULL DEFAULT '',
    parse_meta   JSONB NOT NULL DEFAULT '{}',
    extra        JSONB NOT NULL DEFAULT '{}',
    created_by   TEXT NOT NULL DEFAULT '',
    updated_by   TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_knowledge_documents_hash ON knowledge_documents(tenant_id, content_hash);
CREATE INDEX IF NOT EXISTS idx_knowledge_documents_category ON knowledge_documents(tenant_id, category, id);
CREATE INDEX IF NOT EXISTS idx_knowledge_documents_tags ON knowledge_documents USING GIN(tags);
DROP TRIGGER IF EXISTS trg_knowledge_documents_upd ON knowledge_documents;
CREATE TRIGGER trg_knowledge_documents_upd BEFORE UPDATE ON knowledge_documents
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- 知识库分块。PostgreSQL 内置分词器不切分中文，因此由 Go 层（db.KnowledgeTokens）把 CJK 文本展开为
-- 单字 + 双字 n-gram、拉丁/数字按词小写后写入 search_title / search_body，再以 'simple' 配置生成 tsvector：
-- 标题与章节路径权重 A，正文与表格权重 B。embedding 为可选向量（embedding_model 标识向量空间，只比较同模型向量）。
CREATE TABLE IF NOT EXISTS knowledge_chunks (
    id              BIGSERIAL PRIMARY KEY,
    tenant_id       TEXT NOT NULL DEFAULT 'default',
    doc_id          BIGINT NOT NULL REFERENCES knowledge_documents(id) ON DELETE CASCADE,
    seq             INT  NOT NULL CHECK (seq >= 0),
    level           INT  NOT NULL DEFAULT 0,
    section_code    TEXT NOT NULL DEFAULT '',
    section_title   TEXT NOT NULL DEFAULT '',
    section_path    TEXT NOT NULL DEFAULT '',
    page_start      INT  NOT NULL DEFAULT 1,
    page_end        INT  NOT NULL DEFAULT 1,
    content         TEXT NOT NULL DEFAULT '',
    tables_json     JSONB NOT NULL DEFAULT '[]',
    bbox_meta       JSONB NOT NULL DEFAULT '{}',
    token_estimate  INT  NOT NULL DEFAULT 0,
    is_mandatory    BOOLEAN NOT NULL DEFAULT false,
    search_title    TEXT NOT NULL DEFAULT '',
    search_body     TEXT NOT NULL DEFAULT '',
    search_tsv      tsvector GENERATED ALWAYS AS (
                        setweight(to_tsvector('simple'::regconfig, search_title), 'A') ||
                        setweight(to_tsvector('simple'::regconfig, search_body), 'B')) STORED,
    embedding       float8[],
    embedding_model TEXT NOT NULL DEFAULT '',
    created_by      TEXT NOT NULL DEFAULT '',
    updated_by      TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT ck_knowledge_chunk_pages CHECK (page_end >= page_start)
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_knowledge_chunks_seq ON knowledge_chunks(tenant_id, doc_id, seq);
CREATE INDEX IF NOT EXISTS idx_knowledge_chunks_tsv ON knowledge_chunks USING GIN(search_tsv);
CREATE INDEX IF NOT EXISTS idx_knowledge_chunks_model ON knowledge_chunks(tenant_id, embedding_model) WHERE embedding IS NOT NULL;

-- 余弦相似度（两向量任一为空、维度不同或零向量时返回 0）。
CREATE OR REPLACE FUNCTION kb_cosine(a float8[], b float8[]) RETURNS float8 AS $$
    SELECT CASE
        WHEN a IS NULL OR b IS NULL OR cardinality(a) = 0 OR cardinality(a) <> cardinality(b) THEN 0
        ELSE COALESCE((
            SELECT SUM(x * y) / NULLIF(sqrt(SUM(x * x)) * sqrt(SUM(y * y)), 0)
            FROM unnest(a, b) AS t(x, y)), 0)
    END
$$ LANGUAGE sql IMMUTABLE PARALLEL SAFE;

-- Phase 4.2-4.4: versioned KB indexes (D), retrieval runs/evidence/jobs (B).
CREATE TABLE IF NOT EXISTS knowledge_catalogs (
    tenant_id TEXT PRIMARY KEY DEFAULT 'default',
    epoch BIGINT NOT NULL DEFAULT 0,
    created_by TEXT NOT NULL DEFAULT '', updated_by TEXT NOT NULL DEFAULT '', approved_by TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS knowledge_indexes (
    id BIGSERIAL PRIMARY KEY, tenant_id TEXT NOT NULL DEFAULT 'default',
    model TEXT NOT NULL, dimension INT NOT NULL CHECK (dimension > 0),
    preprocessing TEXT NOT NULL, epoch BIGINT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('building','ready','failed')),
    active BOOLEAN NOT NULL DEFAULT false, error TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by TEXT NOT NULL DEFAULT '', updated_by TEXT NOT NULL DEFAULT '', approved_by TEXT NOT NULL DEFAULT '',
    UNIQUE (tenant_id,id)
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_knowledge_active_index ON knowledge_indexes(tenant_id) WHERE active;
CREATE TABLE IF NOT EXISTS knowledge_embeddings (
    tenant_id TEXT NOT NULL DEFAULT 'default', index_id BIGINT NOT NULL, chunk_id BIGINT NOT NULL REFERENCES knowledge_chunks(id) ON DELETE CASCADE,
    content_hash TEXT NOT NULL, embedding float8[] NOT NULL,
    created_by TEXT NOT NULL DEFAULT '', updated_by TEXT NOT NULL DEFAULT '', approved_by TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (tenant_id,index_id,chunk_id),
    FOREIGN KEY (tenant_id,index_id) REFERENCES knowledge_indexes(tenant_id,id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS retrieval_runs (
    id BIGSERIAL PRIMARY KEY, tenant_id TEXT NOT NULL DEFAULT 'default', task_id BIGINT,
    request_key TEXT NOT NULL, request_hash TEXT NOT NULL, request JSONB NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('search','research')),
    status TEXT NOT NULL DEFAULT 'running' CHECK (status IN ('running','completed','partial','failed','cancelled','budget_exhausted')),
    result JSONB NOT NULL DEFAULT '{}',
    max_calls INT NOT NULL CHECK (max_calls>0), max_tokens INT NOT NULL CHECK (max_tokens>0),
    used_calls INT NOT NULL DEFAULT 0, reserved_tokens INT NOT NULL DEFAULT 0,
    actual_tokens INT NOT NULL DEFAULT 0,
    deadline TIMESTAMPTZ NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT now(), finished_at TIMESTAMPTZ,
    created_by TEXT NOT NULL DEFAULT '', updated_by TEXT NOT NULL DEFAULT '', approved_by TEXT NOT NULL DEFAULT '',
    UNIQUE (tenant_id,request_key), UNIQUE (tenant_id,id)
);
CREATE TABLE IF NOT EXISTS retrieval_evidence (
    id BIGSERIAL PRIMARY KEY, tenant_id TEXT NOT NULL DEFAULT 'default', run_id BIGINT NOT NULL,
    source_domain TEXT NOT NULL CHECK (source_domain IN ('knowledge','tender','credential')),
    source_id BIGINT NOT NULL, doc_id BIGINT NOT NULL, content_hash TEXT NOT NULL,
    snapshot JSONB NOT NULL, claim JSONB NOT NULL DEFAULT '{}',
    relation TEXT NOT NULL DEFAULT 'insufficient' CHECK (relation IN ('supports','contradicts','insufficient')),
    status TEXT NOT NULL DEFAULT 'unverified' CHECK (status IN ('unverified','verified','stale')),
    reason TEXT NOT NULL DEFAULT '', created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by TEXT NOT NULL DEFAULT '', updated_by TEXT NOT NULL DEFAULT '', approved_by TEXT NOT NULL DEFAULT '',
    UNIQUE (tenant_id,run_id,source_domain,source_id),
    FOREIGN KEY (tenant_id,run_id) REFERENCES retrieval_runs(tenant_id,id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS retrieval_jobs (
    id BIGSERIAL PRIMARY KEY, tenant_id TEXT NOT NULL DEFAULT 'default', run_id BIGINT NOT NULL,
    ordinal INT NOT NULL, spec JSONB NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','running','completed','failed','cancelled','budget_exhausted')),
    lease_token BIGINT NOT NULL DEFAULT 0, lease_until TIMESTAMPTZ,
    attempts INT NOT NULL DEFAULT 0, result JSONB NOT NULL DEFAULT '{}',
    created_by TEXT NOT NULL DEFAULT '', updated_by TEXT NOT NULL DEFAULT '', approved_by TEXT NOT NULL DEFAULT '',
    UNIQUE (tenant_id,run_id,ordinal),
    FOREIGN KEY (tenant_id,run_id) REFERENCES retrieval_runs(tenant_id,id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_retrieval_runs_task ON retrieval_runs(tenant_id,task_id,id DESC);
CREATE INDEX IF NOT EXISTS idx_retrieval_evidence_source ON retrieval_evidence(tenant_id,source_domain,doc_id);

CREATE OR REPLACE FUNCTION retrieval_evidence_invalidated() RETURNS trigger AS $$
BEGIN
    IF NEW.status='stale' THEN
        UPDATE retrieval_runs SET status='partial',result=jsonb_build_object('gap','source evidence invalidated; reread current evidence'),updated_by=NEW.updated_by
        WHERE tenant_id=NEW.tenant_id AND id=NEW.run_id AND kind='research' AND status='completed';
    END IF;
    RETURN NEW;
END; $$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS retrieval_evidence_invalidated_trigger ON retrieval_evidence;
CREATE TRIGGER retrieval_evidence_invalidated_trigger AFTER UPDATE OF status ON retrieval_evidence
    FOR EACH ROW EXECUTE FUNCTION retrieval_evidence_invalidated();

CREATE OR REPLACE FUNCTION knowledge_changed() RETURNS trigger AS $$
DECLARE t TEXT; did BIGINT;
BEGIN
    IF TG_OP = 'DELETE' THEN t := OLD.tenant_id; ELSE t := NEW.tenant_id; END IF;
    INSERT INTO knowledge_catalogs(tenant_id,epoch) VALUES(t,1)
        ON CONFLICT(tenant_id) DO UPDATE SET epoch=knowledge_catalogs.epoch+1;
    IF TG_TABLE_NAME='knowledge_documents' THEN
        IF TG_OP='DELETE' THEN did:=OLD.id; ELSE did:=NEW.id; END IF;
    ELSE
        IF TG_OP='DELETE' THEN did:=OLD.doc_id; ELSE did:=NEW.doc_id; END IF;
    END IF;
    UPDATE retrieval_evidence SET status='stale', reason='source changed or deleted'
        WHERE tenant_id=t AND source_domain='knowledge' AND doc_id=did AND status<>'stale';
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_knowledge_documents_changed ON knowledge_documents;
CREATE TRIGGER trg_knowledge_documents_changed AFTER INSERT OR UPDATE OR DELETE ON knowledge_documents
    FOR EACH ROW EXECUTE FUNCTION knowledge_changed();
DROP TRIGGER IF EXISTS trg_knowledge_chunks_changed ON knowledge_chunks;
CREATE TRIGGER trg_knowledge_chunks_changed AFTER INSERT OR UPDATE OR DELETE ON knowledge_chunks
    FOR EACH ROW EXECUTE FUNCTION knowledge_changed();

CREATE OR REPLACE FUNCTION retrieval_source_changed() RETURNS trigger AS $$
DECLARE source TEXT; row_id BIGINT; parent_id BIGINT; t TEXT;
BEGIN
    -- UPDATE OF 列触发器在同值写入时也会触发；这里再比较实际内容，元数据/同值更新不使证据失效。
    IF TG_OP = 'UPDATE' THEN
        IF TG_TABLE_NAME = 'tender_documents' AND
           (to_jsonb(NEW)->>'raw_text', to_jsonb(NEW)->>'title', to_jsonb(NEW)->>'tender_no',
            to_jsonb(NEW)->>'lot_no', to_jsonb(NEW)->>'doc_role', to_jsonb(NEW)->>'published_at')
           IS NOT DISTINCT FROM
           (to_jsonb(OLD)->>'raw_text', to_jsonb(OLD)->>'title', to_jsonb(OLD)->>'tender_no',
            to_jsonb(OLD)->>'lot_no', to_jsonb(OLD)->>'doc_role', to_jsonb(OLD)->>'published_at') THEN
            RETURN NULL;
        END IF;
        IF TG_TABLE_NAME = 'document_chunks' AND
           (to_jsonb(NEW)->>'content', to_jsonb(NEW)->>'section_code', to_jsonb(NEW)->>'section_title',
            to_jsonb(NEW)->>'page_start', to_jsonb(NEW)->>'page_end', to_jsonb(NEW)->>'tables_json')
           IS NOT DISTINCT FROM
           (to_jsonb(OLD)->>'content', to_jsonb(OLD)->>'section_code', to_jsonb(OLD)->>'section_title',
            to_jsonb(OLD)->>'page_start', to_jsonb(OLD)->>'page_end', to_jsonb(OLD)->>'tables_json') THEN
            RETURN NULL;
        END IF;
    END IF;
    row_id:=OLD.id;
    t:=COALESCE(to_jsonb(OLD)->>'tenant_id','default');
    IF TG_TABLE_NAME='company_credentials' THEN source:='credential';parent_id:=row_id;
    ELSIF TG_TABLE_NAME='tender_documents' THEN source:='tender';parent_id:=row_id;
    ELSE source:='tender';parent_id:=OLD.doc_id;
    END IF;
    UPDATE retrieval_evidence SET status='stale',reason='source changed or deleted'
        WHERE tenant_id=t AND source_domain=source AND doc_id=parent_id AND status<>'stale';
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
-- F1：只在实质内容列变化或删除时使检索证据失效。关联原件、parse_revision、page_count、parse_meta、
-- token_estimate、bbox_meta 等元数据更新不得把证据批量标为 stale。
DROP TRIGGER IF EXISTS trg_tender_retrieval_stale ON tender_documents;
CREATE TRIGGER trg_tender_retrieval_stale
    AFTER UPDATE OF raw_text, title, tender_no, lot_no, doc_role, published_at OR DELETE
    ON tender_documents FOR EACH ROW EXECUTE FUNCTION retrieval_source_changed();
DROP TRIGGER IF EXISTS trg_chunks_retrieval_stale ON document_chunks;
CREATE TRIGGER trg_chunks_retrieval_stale
    AFTER UPDATE OF content, section_code, section_title, page_start, page_end, tables_json OR DELETE
    ON document_chunks FOR EACH ROW EXECUTE FUNCTION retrieval_source_changed();
DROP TRIGGER IF EXISTS trg_credentials_retrieval_stale ON company_credentials;
CREATE TRIGGER trg_credentials_retrieval_stale AFTER UPDATE OR DELETE ON company_credentials FOR EACH ROW EXECUTE FUNCTION retrieval_source_changed();

-- ---------------------------------------------------------------------------
-- Phase 5（域 C）：可复核要求清单、版本化大纲持久化审批与并发安全章节写作。
--   requirement_manifests  任务级清单确认状态；requirement_items 的实质增删改或其原文分块变更会递增 epoch，
--                          已确认清单变为 stale，并把 approved / pending_approval 大纲置为 superseded。
--   requirement_items      逐文档遍历提取 + 人工复核的完整要求清单（不是检索 Top-K），绑定版本化原文来源与 4.3 证据。
--   proposal_outlines      大纲版本与审批状态机（draft -> pending_approval -> approved -> superseded），跨重启持久化。
--   proposal_sections      大纲章节、前置依赖与写作租约；要求映射见 proposal_section_requirements（复合外键保证同租户同任务）。
--   section_revisions      章节多版本正文；正文不可篡改，候选稿（stale_outline / conflict_candidate）从不成为当前主版本。
-- requirement_manifests.approved_by 记录确认清单完整的操作人。
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS requirement_manifests (
    tenant_id    TEXT NOT NULL DEFAULT 'default',
    task_id      BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    epoch        BIGINT NOT NULL DEFAULT 1 CHECK (epoch > 0),
    status       TEXT NOT NULL DEFAULT 'draft' CHECK (status IN ('draft','confirmed','stale')),
    confirmed_at TIMESTAMPTZ,
    created_by   TEXT NOT NULL DEFAULT '',
    updated_by   TEXT NOT NULL DEFAULT '',
    approved_by  TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, task_id)
);
DROP TRIGGER IF EXISTS trg_requirement_manifests_upd ON requirement_manifests;
CREATE TRIGGER trg_requirement_manifests_upd BEFORE UPDATE ON requirement_manifests
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
-- D1：投标截止时间（抽取或人工指定；bid_deadline_source 记录原文位置或指定人）、本次投标分包（空数组表示全部分包）与 Bid / No-Bid 决策留痕。
ALTER TABLE requirement_manifests
    ADD COLUMN IF NOT EXISTS bid_deadline        TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS bid_deadline_source JSONB NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS bid_lots            TEXT[] NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS bid_decision        TEXT NOT NULL DEFAULT 'pending'
        CHECK (bid_decision IN ('pending','bid','no_bid')),
    ADD COLUMN IF NOT EXISTS bid_decision_note   TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS bid_decided_by      TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS bid_decided_at      TIMESTAMPTZ;
-- F1：招标原文变化（新文档、分块实质变化、采用重新解析修订）推进 source_epoch；
-- 要求提取在事务内记录它读取到的 source_epoch。source_epoch > extracted_source_epoch 即“待重新提取要求”，
-- 即使清单状态被其他路径改回 confirmed，确认与导出也必须阻断。
ALTER TABLE requirement_manifests
    ADD COLUMN IF NOT EXISTS source_epoch           BIGINT NOT NULL DEFAULT 0 CHECK (source_epoch >= 0),
    ADD COLUMN IF NOT EXISTS extracted_source_epoch BIGINT NOT NULL DEFAULT 0 CHECK (extracted_source_epoch >= 0);

CREATE TABLE IF NOT EXISTS requirement_items (
    id              BIGSERIAL PRIMARY KEY,
    tenant_id       TEXT NOT NULL DEFAULT 'default',
    task_id         BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    code            TEXT NOT NULL CHECK (code <> ''),
    category        TEXT NOT NULL
                      CHECK (category IN ('scoring_item','mandatory_clause','non_scoring_required','format_attachment')),
    title           TEXT NOT NULL CHECK (title <> ''),
    description     TEXT NOT NULL DEFAULT '',
    is_mandatory    BOOLEAN NOT NULL DEFAULT false,
    score_weight    DOUBLE PRECISION NOT NULL DEFAULT 0 CHECK (score_weight >= 0),
    origin          TEXT NOT NULL DEFAULT 'manual' CHECK (origin IN ('extracted','manual')),
    source_asset_id BIGINT REFERENCES assets(id) ON DELETE SET NULL,
    source_doc_id   BIGINT REFERENCES tender_documents(id) ON DELETE SET NULL,
    source_chunk_id BIGINT REFERENCES document_chunks(id) ON DELETE SET NULL,
    source_hash     TEXT NOT NULL DEFAULT '',
    source_loc      JSONB NOT NULL DEFAULT '{}',
    evidence_run_id BIGINT,
    evidence_ids    BIGINT[] NOT NULL DEFAULT '{}',
    evidence_gap    TEXT NOT NULL DEFAULT '',
    review_status   TEXT NOT NULL DEFAULT 'draft' CHECK (review_status IN ('draft','confirmed','needs_revision')),
    created_by      TEXT NOT NULL DEFAULT '',
    updated_by      TEXT NOT NULL DEFAULT '',
    approved_by     TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_requirement_items_code UNIQUE (tenant_id, task_id, code),
    CONSTRAINT uq_requirement_items_tenant_id UNIQUE (tenant_id, id),
    CONSTRAINT uq_requirement_items_task_id UNIQUE (tenant_id, task_id, id),
    CONSTRAINT fk_requirement_items_run FOREIGN KEY (tenant_id, evidence_run_id)
        REFERENCES retrieval_runs(tenant_id, id) ON DELETE SET NULL (evidence_run_id)
);
-- 评分项分组（评分表结构化提取）：technical / commercial / price，空串为未分类。price 由报价文件响应，不进入技术标大纲与响应覆盖。
ALTER TABLE requirement_items ADD COLUMN IF NOT EXISTS score_group TEXT NOT NULL DEFAULT ''
    CHECK (score_group IN ('','technical','commercial','price'));
-- 否决项（D1）：资格性 / 符合性审查表逐行与 87 号令法定无效情形。不进入技术标大纲；投标方逐项核对，未通过即阻断导出。
ALTER TABLE requirement_items DROP CONSTRAINT IF EXISTS requirement_items_category_check;
ALTER TABLE requirement_items ADD CONSTRAINT requirement_items_category_check
    CHECK (category IN ('scoring_item','mandatory_clause','non_scoring_required','format_attachment','veto_item'));
ALTER TABLE requirement_items
    ADD COLUMN IF NOT EXISTS veto_kind       TEXT NOT NULL DEFAULT '' CHECK (veto_kind IN ('','qualification','conformity','statutory')),
    ADD COLUMN IF NOT EXISTS veto_status     TEXT NOT NULL DEFAULT 'pending' CHECK (veto_status IN ('pending','passed','failed')),
    ADD COLUMN IF NOT EXISTS veto_note       TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS veto_checked_by TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS veto_checked_at TIMESTAMPTZ,
    -- 分包：lot 为要求所属分包（空串为不分包）；out_of_bid 表示不在本次投标分包内，随 requirement_manifests.bid_lots 维护。
    ADD COLUMN IF NOT EXISTS lot             TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS out_of_bid      BOOLEAN NOT NULL DEFAULT false;
UPDATE requirement_items SET lot = source_loc->>'lot' WHERE lot = '' AND COALESCE(source_loc->>'lot', '') <> '';
-- D1：后发澄清/更正逐条覆盖（superseded_by_id + review_status='stale'）、原子粒度拆解（atom_type / parent_id / score_min / eval_kind / scoring_tiers / proof_rules）与关联采购需求条目（linked_requirement_ids）。
ALTER TABLE requirement_items DROP CONSTRAINT IF EXISTS requirement_items_review_status_check;
ALTER TABLE requirement_items ADD CONSTRAINT requirement_items_review_status_check
    CHECK (review_status IN ('draft','confirmed','needs_revision','stale'));
ALTER TABLE requirement_items
    ADD COLUMN IF NOT EXISTS atom_type              TEXT NOT NULL DEFAULT ''
        CHECK (atom_type IN ('','veto_item','scoring_item','scoring_point','tier_criterion','demand_item','proof_rule','format_rule')),
    ADD COLUMN IF NOT EXISTS parent_id              BIGINT REFERENCES requirement_items(id) ON DELETE CASCADE,
    ADD COLUMN IF NOT EXISTS score_min              DOUBLE PRECISION NOT NULL DEFAULT 0 CHECK (score_min >= 0),
    ADD COLUMN IF NOT EXISTS eval_kind              TEXT NOT NULL DEFAULT ''
        CHECK (eval_kind IN ('','objective','subjective','mixed')),
    ADD COLUMN IF NOT EXISTS scoring_tiers          JSONB NOT NULL DEFAULT '[]',
    ADD COLUMN IF NOT EXISTS proof_rules            JSONB NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS linked_requirement_ids BIGINT[] NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS superseded_by_id       BIGINT REFERENCES requirement_items(id) ON DELETE SET NULL;
-- 否决项内容变化后，此前的核对结论不再适用：退回 pending。
CREATE OR REPLACE FUNCTION requirement_veto_reset() RETURNS trigger AS $$
BEGIN
    IF NEW.category = 'veto_item' AND (NEW.category, NEW.title, NEW.description, NEW.veto_kind, NEW.source_hash)
        IS DISTINCT FROM (OLD.category, OLD.title, OLD.description, OLD.veto_kind, OLD.source_hash) THEN
        NEW.veto_status := 'pending';
        NEW.veto_note := '';
        NEW.veto_checked_by := '';
        NEW.veto_checked_at := NULL;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_requirement_veto_reset ON requirement_items;
CREATE TRIGGER trg_requirement_veto_reset BEFORE UPDATE ON requirement_items
    FOR EACH ROW EXECUTE FUNCTION requirement_veto_reset();
CREATE INDEX IF NOT EXISTS idx_requirement_items_task ON requirement_items(tenant_id, task_id, category, id);
CREATE INDEX IF NOT EXISTS idx_requirement_items_chunk ON requirement_items(source_chunk_id) WHERE source_chunk_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_requirement_items_evidence ON requirement_items USING GIN(evidence_ids);
DROP TRIGGER IF EXISTS trg_requirement_items_upd ON requirement_items;
CREATE TRIGGER trg_requirement_items_upd BEFORE UPDATE ON requirement_items
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE IF NOT EXISTS proposal_outlines (
    id                   BIGSERIAL PRIMARY KEY,
    tenant_id            TEXT NOT NULL DEFAULT 'default',
    task_id              BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    version              INT NOT NULL CHECK (version > 0),
    requirements_epoch   BIGINT NOT NULL DEFAULT 1,
    status               TEXT NOT NULL DEFAULT 'draft'
                           CHECK (status IN ('draft','pending_approval','approved','superseded')),
    derived_from_version INT,
    note                 TEXT NOT NULL DEFAULT '',
    submitted_by         TEXT NOT NULL DEFAULT '',
    submitted_at         TIMESTAMPTZ,
    approved_at          TIMESTAMPTZ,
    superseded_at        TIMESTAMPTZ,
    created_by           TEXT NOT NULL DEFAULT '',
    updated_by           TEXT NOT NULL DEFAULT '',
    approved_by          TEXT NOT NULL DEFAULT '',
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_proposal_outlines_version UNIQUE (tenant_id, task_id, version),
    CONSTRAINT uq_proposal_outlines_tenant_id UNIQUE (tenant_id, id),
    CONSTRAINT uq_proposal_outlines_task_id UNIQUE (tenant_id, task_id, id)
);
-- 每个任务至多一个有效批准大纲、至多一个在编（draft / pending_approval）大纲。
CREATE UNIQUE INDEX IF NOT EXISTS uq_proposal_outlines_approved ON proposal_outlines(tenant_id, task_id) WHERE status = 'approved';
CREATE UNIQUE INDEX IF NOT EXISTS uq_proposal_outlines_open ON proposal_outlines(tenant_id, task_id) WHERE status IN ('draft','pending_approval');
DROP TRIGGER IF EXISTS trg_proposal_outlines_upd ON proposal_outlines;
CREATE TRIGGER trg_proposal_outlines_upd BEFORE UPDATE ON proposal_outlines
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE IF NOT EXISTS proposal_sections (
    id                      BIGSERIAL PRIMARY KEY,
    tenant_id               TEXT NOT NULL DEFAULT 'default',
    task_id                 BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    outline_id              BIGINT NOT NULL,
    outline_version         INT NOT NULL CHECK (outline_version > 0),
    section_code            TEXT NOT NULL CHECK (section_code <> ''),
    title                   TEXT NOT NULL CHECK (title <> ''),
    level                   INT NOT NULL DEFAULT 1 CHECK (level > 0),
    seq                     INT NOT NULL CHECK (seq >= 0),
    writing_guide           TEXT NOT NULL DEFAULT '',
    target_words            INT NOT NULL DEFAULT 0 CHECK (target_words >= 0),
    depends_on_section_ids  BIGINT[] NOT NULL DEFAULT '{}',
    derived_from_section_id BIGINT,
    current_rev_no          INT NOT NULL DEFAULT 0 CHECK (current_rev_no >= 0),
    approved_rev_no         INT,
    status                  TEXT NOT NULL DEFAULT 'pending'
                              CHECK (status IN ('pending','ready','writing','draft','reviewing','approved','rejected','stale_outline','stale_evidence')),
    lease_token             BIGINT NOT NULL DEFAULT 0,
    lease_until             TIMESTAMPTZ,
    created_by              TEXT NOT NULL DEFAULT '',
    updated_by              TEXT NOT NULL DEFAULT '',
    approved_by             TEXT NOT NULL DEFAULT '',
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_proposal_sections_code UNIQUE (tenant_id, outline_id, section_code),
    CONSTRAINT uq_proposal_sections_seq UNIQUE (tenant_id, outline_id, seq),
    CONSTRAINT uq_proposal_sections_tenant_id UNIQUE (tenant_id, id),
    CONSTRAINT uq_proposal_sections_task_id UNIQUE (tenant_id, task_id, id),
    CONSTRAINT ck_proposal_sections_approved CHECK (approved_rev_no IS NULL OR (approved_rev_no > 0 AND approved_rev_no <= current_rev_no)),
    CONSTRAINT fk_proposal_sections_outline FOREIGN KEY (tenant_id, task_id, outline_id)
        REFERENCES proposal_outlines(tenant_id, task_id, id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_proposal_sections_outline ON proposal_sections(tenant_id, outline_id, seq);
DROP TRIGGER IF EXISTS trg_proposal_sections_upd ON proposal_sections;
CREATE TRIGGER trg_proposal_sections_upd BEFORE UPDATE ON proposal_sections
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- 前置依赖只能指向同一大纲内的其他章节（无环由 Go 层在同一事务内校验）。
CREATE OR REPLACE FUNCTION proposal_sections_check_deps() RETURNS trigger AS $$
BEGIN
    IF NEW.id = ANY(NEW.depends_on_section_ids) THEN
        RAISE EXCEPTION 'proposal section % depends on itself', NEW.id USING ERRCODE = 'check_violation';
    END IF;
    IF EXISTS (SELECT 1 FROM unnest(NEW.depends_on_section_ids) AS d(id)
               WHERE NOT EXISTS (SELECT 1 FROM proposal_sections s
                                 WHERE s.id = d.id AND s.tenant_id = NEW.tenant_id AND s.outline_id = NEW.outline_id)) THEN
        RAISE EXCEPTION 'proposal section dependencies must belong to outline %', NEW.outline_id USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_proposal_sections_deps ON proposal_sections;
CREATE TRIGGER trg_proposal_sections_deps BEFORE INSERT OR UPDATE OF depends_on_section_ids ON proposal_sections
    FOR EACH ROW EXECUTE FUNCTION proposal_sections_check_deps();

-- 章节与要求的多对多映射：复合外键保证两端同租户、同任务，杜绝跨租户 / 跨任务引用。
CREATE TABLE IF NOT EXISTS proposal_section_requirements (
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    task_id        BIGINT NOT NULL,
    section_id     BIGINT NOT NULL,
    requirement_id BIGINT NOT NULL,
    created_by     TEXT NOT NULL DEFAULT '',
    updated_by     TEXT NOT NULL DEFAULT '',
    approved_by    TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, section_id, requirement_id),
    FOREIGN KEY (tenant_id, task_id, section_id) REFERENCES proposal_sections(tenant_id, task_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id, task_id, requirement_id) REFERENCES requirement_items(tenant_id, task_id, id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_proposal_section_requirements_req ON proposal_section_requirements(tenant_id, requirement_id);

CREATE TABLE IF NOT EXISTS section_revisions (
    id                       BIGSERIAL PRIMARY KEY,
    tenant_id                TEXT NOT NULL DEFAULT 'default',
    task_id                  BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    section_id               BIGINT NOT NULL,
    rev_no                   INT NOT NULL CHECK (rev_no > 0),
    base_rev_no              INT NOT NULL CHECK (base_rev_no >= 0),
    based_on_outline_version INT NOT NULL CHECK (based_on_outline_version > 0),
    source                   TEXT NOT NULL CHECK (source IN ('ai_initial','ai_rewrite','human_edit','rollback')),
    rollback_of_rev_no       INT,
    content_md               TEXT NOT NULL DEFAULT '',
    summary_note             TEXT NOT NULL DEFAULT '',
    diff_summary             TEXT NOT NULL DEFAULT '',
    evidence_ids             BIGINT[] NOT NULL DEFAULT '{}',
    gaps                     JSONB NOT NULL DEFAULT '[]',
    status                   TEXT NOT NULL
                               CHECK (status IN ('draft','reviewing','approved','rejected','stale_outline','stale_evidence','conflict_candidate')),
    created_by               TEXT NOT NULL DEFAULT '',
    updated_by               TEXT NOT NULL DEFAULT '',
    approved_by              TEXT NOT NULL DEFAULT '',
    approved_at              TIMESTAMPTZ,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_section_revisions_rev UNIQUE (tenant_id, section_id, rev_no),
    FOREIGN KEY (tenant_id, task_id, section_id) REFERENCES proposal_sections(tenant_id, task_id, id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_section_revisions_evidence ON section_revisions USING GIN(evidence_ids);
DROP TRIGGER IF EXISTS trg_section_revisions_upd ON section_revisions;
CREATE TRIGGER trg_section_revisions_upd BEFORE UPDATE ON section_revisions
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- 版本链不可篡改：修订记录只允许变更状态与审批字段。
CREATE OR REPLACE FUNCTION section_revisions_immutable() RETURNS trigger AS $$
BEGIN
    IF (NEW.tenant_id, NEW.task_id, NEW.section_id, NEW.rev_no, NEW.base_rev_no, NEW.based_on_outline_version, NEW.source,
        NEW.rollback_of_rev_no, NEW.content_md, NEW.summary_note, NEW.diff_summary, NEW.evidence_ids, NEW.gaps, NEW.created_by, NEW.created_at)
       IS DISTINCT FROM
       (OLD.tenant_id, OLD.task_id, OLD.section_id, OLD.rev_no, OLD.base_rev_no, OLD.based_on_outline_version, OLD.source,
        OLD.rollback_of_rev_no, OLD.content_md, OLD.summary_note, OLD.diff_summary, OLD.evidence_ids, OLD.gaps, OLD.created_by, OLD.created_at) THEN
        RAISE EXCEPTION 'section revision % is immutable', OLD.id USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_section_revisions_immutable ON section_revisions;
CREATE TRIGGER trg_section_revisions_immutable BEFORE UPDATE ON section_revisions
    FOR EACH ROW EXECUTE FUNCTION section_revisions_immutable();

-- 要求清单失效：递增清单 epoch，已确认清单变 stale，审批中 / 已批准大纲置为 superseded。
-- 任务正在级联删除时（tasks 行已不可见）不做任何事。
CREATE OR REPLACE FUNCTION requirement_manifest_bump(t TEXT, tid BIGINT, actor TEXT) RETURNS void AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM tasks WHERE id = tid) THEN
        RETURN;
    END IF;
    INSERT INTO requirement_manifests(tenant_id, task_id, epoch, status, created_by, updated_by)
    VALUES (t, tid, 1, 'draft', actor, actor)
    ON CONFLICT (tenant_id, task_id) DO UPDATE
        SET epoch = requirement_manifests.epoch + 1,
            status = CASE WHEN requirement_manifests.status = 'confirmed' THEN 'stale' ELSE requirement_manifests.status END,
            updated_by = EXCLUDED.updated_by;
    UPDATE proposal_outlines SET status = 'superseded', superseded_at = now(), updated_by = actor
        WHERE tenant_id = t AND task_id = tid AND status IN ('approved','pending_approval');
END;
$$ LANGUAGE plpgsql;

-- 招标原文实质变化：推进 source_epoch，并复用清单失效（epoch 递增、已确认清单 stale、审批中/已批准大纲 superseded）。
-- 与 requirement_manifest_bump 分开，避免证据绑定、否决项核对等非原文变化把“待重新提取”状态错误置位。
CREATE OR REPLACE FUNCTION requirement_source_bump(t TEXT, tid BIGINT, actor TEXT) RETURNS void AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM tasks WHERE id = tid) THEN
        RETURN;
    END IF;
    INSERT INTO requirement_manifests(tenant_id, task_id, epoch, status, source_epoch, created_by, updated_by)
    VALUES (t, tid, 1, 'draft', 1, actor, actor)
    ON CONFLICT (tenant_id, task_id) DO UPDATE
        SET source_epoch = requirement_manifests.source_epoch + 1,
            updated_by = EXCLUDED.updated_by;
    PERFORM requirement_manifest_bump(t, tid, actor);
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION requirement_items_changed() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        PERFORM requirement_manifest_bump(OLD.tenant_id, OLD.task_id, OLD.updated_by);
        RETURN NULL;
    END IF;
    IF TG_OP = 'UPDATE' AND
       (NEW.code, NEW.category, NEW.title, NEW.description, NEW.is_mandatory, NEW.score_weight, NEW.score_group,
        NEW.source_doc_id, NEW.source_chunk_id, NEW.source_hash, NEW.source_loc, NEW.veto_kind, NEW.out_of_bid,
        NEW.superseded_by_id, NEW.parent_id, NEW.score_min, NEW.eval_kind, NEW.scoring_tiers, NEW.proof_rules)
       IS NOT DISTINCT FROM
       (OLD.code, OLD.category, OLD.title, OLD.description, OLD.is_mandatory, OLD.score_weight, OLD.score_group,
        OLD.source_doc_id, OLD.source_chunk_id, OLD.source_hash, OLD.source_loc, OLD.veto_kind, OLD.out_of_bid,
        OLD.superseded_by_id, OLD.parent_id, OLD.score_min, OLD.eval_kind, OLD.scoring_tiers, OLD.proof_rules) THEN
        RETURN NULL; -- 证据绑定与复核状态变化不改变清单内容
    END IF;
    PERFORM requirement_manifest_bump(NEW.tenant_id, NEW.task_id, NEW.updated_by);
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_requirement_items_changed ON requirement_items;
CREATE TRIGGER trg_requirement_items_changed AFTER INSERT OR UPDATE OR DELETE ON requirement_items
    FOR EACH ROW EXECUTE FUNCTION requirement_items_changed();

-- 要求的招标原文分块被修改或删除：要求标记待复核，并推进 source_epoch / 清单失效
-- （BEFORE DELETE 先于 SET NULL 外键动作）。即使该分块当前没有关联要求，也必须推进完整性 epoch，
-- 防止“仅新增/变化无关联要求的分块”绕过重新提取门禁。
CREATE OR REPLACE FUNCTION requirement_source_changed() RETURNS trigger AS $$
DECLARE t TEXT;
BEGIN
    IF TG_OP = 'UPDATE' AND (NEW.content, NEW.section_code, NEW.section_title, NEW.page_start, NEW.page_end, NEW.tables_json)
        IS NOT DISTINCT FROM (OLD.content, OLD.section_code, OLD.section_title, OLD.page_start, OLD.page_end, OLD.tables_json) THEN
        RETURN NULL;
    END IF;
    IF EXISTS (SELECT 1 FROM tasks WHERE id = OLD.task_id) THEN
        SELECT tenant_id INTO t FROM tender_documents WHERE id = OLD.doc_id;
        UPDATE requirement_items SET review_status = 'needs_revision'
            WHERE source_chunk_id = OLD.id;
        IF t IS NOT NULL THEN
            PERFORM requirement_source_bump(t, OLD.task_id, 'system');
        END IF;
    END IF;
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_chunks_requirement_upd ON document_chunks;
CREATE TRIGGER trg_chunks_requirement_upd AFTER UPDATE ON document_chunks
    FOR EACH ROW EXECUTE FUNCTION requirement_source_changed();
DROP TRIGGER IF EXISTS trg_chunks_requirement_del ON document_chunks;
CREATE TRIGGER trg_chunks_requirement_del BEFORE DELETE ON document_chunks
    FOR EACH ROW EXECUTE FUNCTION requirement_source_changed();

-- 大纲被取代：其章节失去写作资格（租约作废），在途写回只能归档为 stale_outline 候选稿。
CREATE OR REPLACE FUNCTION proposal_outline_superseded() RETURNS trigger AS $$
BEGIN
    UPDATE proposal_sections SET status = 'stale_outline', lease_until = NULL, updated_by = NEW.updated_by
        WHERE tenant_id = NEW.tenant_id AND outline_id = NEW.id;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_proposal_outline_superseded ON proposal_outlines;
CREATE TRIGGER trg_proposal_outline_superseded AFTER UPDATE OF status ON proposal_outlines
    FOR EACH ROW WHEN (NEW.status = 'superseded' AND OLD.status <> 'superseded')
    EXECUTE FUNCTION proposal_outline_superseded();

-- Phase 4.3 证据失效联动：引用该证据的修订标记 stale_evidence；章节当前主版本受影响时撤销批准并降级；
-- 要求项记录证据缺口。失效证据不能继续支撑定稿。
CREATE OR REPLACE FUNCTION proposal_evidence_invalidated() RETURNS trigger AS $$
BEGIN
    IF NEW.status <> 'stale' OR OLD.status = 'stale' THEN
        RETURN NULL;
    END IF;
    UPDATE section_revisions SET status = 'stale_evidence', updated_by = 'system'
        WHERE tenant_id = NEW.tenant_id AND evidence_ids @> ARRAY[NEW.id]
          AND status IN ('draft','reviewing','approved');
    UPDATE proposal_sections s SET status = 'stale_evidence', approved_rev_no = NULL, updated_by = 'system'
        FROM section_revisions r
        WHERE r.tenant_id = NEW.tenant_id AND r.section_id = s.id AND r.rev_no = s.current_rev_no
          AND r.evidence_ids @> ARRAY[NEW.id] AND s.tenant_id = NEW.tenant_id AND s.status <> 'stale_outline';
    UPDATE requirement_items
        SET evidence_gap = btrim(evidence_gap || E'\n' || format('evidence %s stale: %s', NEW.id, NEW.reason))
        WHERE tenant_id = NEW.tenant_id AND evidence_ids @> ARRAY[NEW.id];
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_proposal_evidence_invalidated ON retrieval_evidence;
CREATE TRIGGER trg_proposal_evidence_invalidated AFTER UPDATE OF status ON retrieval_evidence
    FOR EACH ROW EXECUTE FUNCTION proposal_evidence_invalidated();

-- ---------------------------------------------------------------------------
-- Phase 6A（标书正文域，与 explorations/findings 推演域严格隔离）：双层响应核验矩阵与全文一致性核验。
--   compliance_matrix     要求 × 已批准大纲章节的逐条正文实质响应核验结果，绑定被核验的具体修订（section_rev_id + rev_no）
--                         与段落锚点；行只能挂在 proposal_section_requirements 已有映射上（第一层映射不另建）。
--   consistency_runs      一次全文一致性核验：输入快照（section_id -> rev_no）与汇总结论。
--   consistency_findings  矛盾 / 漏项结论，记录双方章节、rev_no、原文片段与处理人；version 为处理乐观锁。
-- 失效全部在 DB 层：要求实质变更、章节产生新有效修订、引用证据 stale、大纲 superseded -> 矩阵行 / 结论 stale；
-- 写入守卫保证不会把旧正文的结论写到新修订上（修订已变、大纲非 approved、证据 stale 的写入一律落为 stale）。
-- 6B 的导出门禁与点对点应答偏离表直接读取这三张表。
-- ---------------------------------------------------------------------------
CREATE UNIQUE INDEX IF NOT EXISTS uq_section_revisions_section_rev ON section_revisions(tenant_id, section_id, id, rev_no);
CREATE UNIQUE INDEX IF NOT EXISTS uq_proposal_sections_outline_id ON proposal_sections(tenant_id, outline_id, id);

CREATE TABLE IF NOT EXISTS compliance_matrix (
    id                  BIGSERIAL PRIMARY KEY,
    tenant_id           TEXT NOT NULL DEFAULT 'default',
    task_id             BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    outline_id          BIGINT NOT NULL,
    outline_version     INT NOT NULL CHECK (outline_version > 0),
    requirement_item_id BIGINT NOT NULL,
    section_id          BIGINT NOT NULL,
    section_rev_id      BIGINT,
    rev_no              INT,
    anchor              TEXT NOT NULL DEFAULT '',
    anchor_excerpt      TEXT NOT NULL DEFAULT '',
    response_summary    TEXT NOT NULL DEFAULT '',
    deviation           TEXT NOT NULL DEFAULT 'none' CHECK (deviation IN ('none','positive','negative')),
    verify_status       TEXT NOT NULL DEFAULT 'pending' CHECK (verify_status IN ('pending','verified','stale','failed')),
    failure_reason      TEXT NOT NULL DEFAULT '',
    stale_reason        TEXT NOT NULL DEFAULT '',
    evidence_ids        BIGINT[] NOT NULL DEFAULT '{}',
    checks              JSONB NOT NULL DEFAULT '[]',
    verifier            TEXT NOT NULL DEFAULT 'rule' CHECK (verifier IN ('rule','llm')),
    verified_at         TIMESTAMPTZ,
    created_by          TEXT NOT NULL DEFAULT '',
    updated_by          TEXT NOT NULL DEFAULT '',
    approved_by         TEXT NOT NULL DEFAULT '',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_compliance_matrix_pair UNIQUE (tenant_id, section_id, requirement_item_id),
    CONSTRAINT uq_compliance_matrix_task_id UNIQUE (tenant_id, task_id, id),
    CONSTRAINT ck_compliance_matrix_rev CHECK ((section_rev_id IS NULL) = (rev_no IS NULL)),
    CONSTRAINT ck_compliance_matrix_bound CHECK (verify_status IN ('pending','stale') OR section_rev_id IS NOT NULL),
    CONSTRAINT fk_compliance_matrix_outline FOREIGN KEY (tenant_id, task_id, outline_id)
        REFERENCES proposal_outlines(tenant_id, task_id, id) ON DELETE CASCADE,
    CONSTRAINT fk_compliance_matrix_section FOREIGN KEY (tenant_id, outline_id, section_id)
        REFERENCES proposal_sections(tenant_id, outline_id, id) ON DELETE CASCADE,
    CONSTRAINT fk_compliance_matrix_requirement FOREIGN KEY (tenant_id, task_id, requirement_item_id)
        REFERENCES requirement_items(tenant_id, task_id, id) ON DELETE CASCADE,
    CONSTRAINT fk_compliance_matrix_mapping FOREIGN KEY (tenant_id, section_id, requirement_item_id)
        REFERENCES proposal_section_requirements(tenant_id, section_id, requirement_id) ON DELETE CASCADE,
    CONSTRAINT fk_compliance_matrix_revision FOREIGN KEY (tenant_id, section_id, section_rev_id, rev_no)
        REFERENCES section_revisions(tenant_id, section_id, id, rev_no) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_compliance_matrix_task ON compliance_matrix(tenant_id, task_id, outline_id, verify_status);
CREATE INDEX IF NOT EXISTS idx_compliance_matrix_req ON compliance_matrix(tenant_id, requirement_item_id);
CREATE INDEX IF NOT EXISTS idx_compliance_matrix_evidence ON compliance_matrix USING GIN(evidence_ids);
DROP TRIGGER IF EXISTS trg_compliance_matrix_upd ON compliance_matrix;
CREATE TRIGGER trg_compliance_matrix_upd BEFORE UPDATE ON compliance_matrix
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE IF NOT EXISTS consistency_runs (
    id              BIGSERIAL PRIMARY KEY,
    tenant_id       TEXT NOT NULL DEFAULT 'default',
    task_id         BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    outline_id      BIGINT NOT NULL,
    outline_version INT NOT NULL CHECK (outline_version > 0),
    snapshot        JSONB NOT NULL DEFAULT '{}',
    status          TEXT NOT NULL DEFAULT 'completed' CHECK (status IN ('completed','stale')),
    stale_reason    TEXT NOT NULL DEFAULT '',
    contradictions  INT NOT NULL DEFAULT 0 CHECK (contradictions >= 0),
    omissions       INT NOT NULL DEFAULT 0 CHECK (omissions >= 0),
    summary         JSONB NOT NULL DEFAULT '{}',
    created_by      TEXT NOT NULL DEFAULT '',
    updated_by      TEXT NOT NULL DEFAULT '',
    approved_by     TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_consistency_runs_task_id UNIQUE (tenant_id, task_id, id),
    CONSTRAINT fk_consistency_runs_outline FOREIGN KEY (tenant_id, task_id, outline_id)
        REFERENCES proposal_outlines(tenant_id, task_id, id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_consistency_runs_task ON consistency_runs(tenant_id, task_id, id DESC);
DROP TRIGGER IF EXISTS trg_consistency_runs_upd ON consistency_runs;
CREATE TRIGGER trg_consistency_runs_upd BEFORE UPDATE ON consistency_runs
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE IF NOT EXISTS consistency_findings (
    id                  BIGSERIAL PRIMARY KEY,
    tenant_id           TEXT NOT NULL DEFAULT 'default',
    task_id             BIGINT NOT NULL,
    run_id              BIGINT NOT NULL,
    kind                TEXT NOT NULL CHECK (kind IN ('contradiction','omission')),
    metric              TEXT NOT NULL DEFAULT '',
    metric_label        TEXT NOT NULL DEFAULT '',
    requirement_item_id BIGINT,
    section_ids         BIGINT[] NOT NULL DEFAULT '{}',
    a_section_id        BIGINT,
    a_rev_no            INT,
    a_value             TEXT NOT NULL DEFAULT '',
    a_excerpt           TEXT NOT NULL DEFAULT '',
    b_section_id        BIGINT,
    b_rev_no            INT,
    b_value             TEXT NOT NULL DEFAULT '',
    b_excerpt           TEXT NOT NULL DEFAULT '',
    detail              TEXT NOT NULL DEFAULT '',
    status              TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open','resolved','stale')),
    stale_reason        TEXT NOT NULL DEFAULT '',
    resolution_kind     TEXT NOT NULL DEFAULT '' CHECK (resolution_kind IN ('','fixed','accepted','false_positive')),
    resolution_note     TEXT NOT NULL DEFAULT '',
    resolved_by         TEXT NOT NULL DEFAULT '',
    resolved_at         TIMESTAMPTZ,
    version             INT NOT NULL DEFAULT 1 CHECK (version > 0),
    created_by          TEXT NOT NULL DEFAULT '',
    updated_by          TEXT NOT NULL DEFAULT '',
    approved_by         TEXT NOT NULL DEFAULT '',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT ck_consistency_findings_shape CHECK (
        (kind = 'contradiction' AND a_section_id IS NOT NULL AND b_section_id IS NOT NULL AND a_rev_no IS NOT NULL AND b_rev_no IS NOT NULL)
        OR (kind = 'omission' AND requirement_item_id IS NOT NULL)),
    CONSTRAINT fk_consistency_findings_run FOREIGN KEY (tenant_id, task_id, run_id)
        REFERENCES consistency_runs(tenant_id, task_id, id) ON DELETE CASCADE,
    CONSTRAINT fk_consistency_findings_requirement FOREIGN KEY (tenant_id, task_id, requirement_item_id)
        REFERENCES requirement_items(tenant_id, task_id, id) ON DELETE CASCADE,
    CONSTRAINT fk_consistency_findings_a FOREIGN KEY (tenant_id, task_id, a_section_id)
        REFERENCES proposal_sections(tenant_id, task_id, id) ON DELETE CASCADE,
    CONSTRAINT fk_consistency_findings_b FOREIGN KEY (tenant_id, task_id, b_section_id)
        REFERENCES proposal_sections(tenant_id, task_id, id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_consistency_findings_run ON consistency_findings(tenant_id, run_id, kind, id);
CREATE INDEX IF NOT EXISTS idx_consistency_findings_sections ON consistency_findings USING GIN(section_ids);
DROP TRIGGER IF EXISTS trg_consistency_findings_upd ON consistency_findings;
CREATE TRIGGER trg_consistency_findings_upd BEFORE UPDATE ON consistency_findings
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- 写入守卫：非 stale 的矩阵行必须对应章节当前主版本、所属大纲仍为 approved、引用证据均未失效，否则强制落为 stale。
-- PL/pgSQL 查询在 READ COMMITTED 下每条语句取新快照，因此等锁后写入也能看到并发提交的修订 / 证据失效。
CREATE OR REPLACE FUNCTION compliance_matrix_guard() RETURNS trigger AS $$
DECLARE cur INT; ost TEXT;
BEGIN
    IF NEW.verify_status = 'stale' THEN
        RETURN NEW;
    END IF;
    SELECT s.current_rev_no, o.status INTO cur, ost
        FROM proposal_sections s JOIN proposal_outlines o ON o.tenant_id = s.tenant_id AND o.id = s.outline_id
        WHERE s.tenant_id = NEW.tenant_id AND s.id = NEW.section_id;
    IF ost IS DISTINCT FROM 'approved' THEN
        NEW.verify_status := 'stale';
        NEW.stale_reason := 'outline is no longer the approved version';
    ELSIF NEW.rev_no IS DISTINCT FROM NULLIF(cur, 0) THEN
        NEW.verify_status := 'stale';
        NEW.stale_reason := format('verified rev %s but section current_rev_no is %s', COALESCE(NEW.rev_no::text, 'none'), cur);
    ELSIF EXISTS (SELECT 1 FROM unnest(NEW.evidence_ids) AS e(id)
                  LEFT JOIN retrieval_evidence x ON x.tenant_id = NEW.tenant_id AND x.id = e.id
                  WHERE x.id IS NULL OR x.status = 'stale') THEN
        NEW.verify_status := 'stale';
        NEW.stale_reason := 'referenced evidence is stale or missing';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_compliance_matrix_guard ON compliance_matrix;
CREATE TRIGGER trg_compliance_matrix_guard BEFORE INSERT OR UPDATE ON compliance_matrix
    FOR EACH ROW EXECUTE FUNCTION compliance_matrix_guard();

-- 失效路径 1：要求实质变更或被补遗覆盖 -> 该要求的矩阵行与漏项结论 stale，关联已批准章节降级需重审（触发器名排在 trg_requirement_items_changed 之前，先记录要求原因）。
CREATE OR REPLACE FUNCTION compliance_requirement_changed() RETURNS trigger AS $$
BEGIN
    IF (NEW.code, NEW.category, NEW.title, NEW.description, NEW.is_mandatory, NEW.score_weight, NEW.score_group,
        NEW.source_doc_id, NEW.source_chunk_id, NEW.source_hash, NEW.source_loc, NEW.veto_kind, NEW.out_of_bid,
        NEW.superseded_by_id, NEW.parent_id, NEW.score_min, NEW.eval_kind, NEW.scoring_tiers, NEW.proof_rules)
       IS NOT DISTINCT FROM
       (OLD.code, OLD.category, OLD.title, OLD.description, OLD.is_mandatory, OLD.score_weight, OLD.score_group,
        OLD.source_doc_id, OLD.source_chunk_id, OLD.source_hash, OLD.source_loc, OLD.veto_kind, OLD.out_of_bid,
        OLD.superseded_by_id, OLD.parent_id, OLD.score_min, OLD.eval_kind, OLD.scoring_tiers, OLD.proof_rules) THEN
        RETURN NULL;
    END IF;
    UPDATE proposal_sections s
        SET status = CASE WHEN s.status = 'approved' THEN 'reviewing' ELSE s.status END,
            approved_rev_no = NULL,
            updated_by = NEW.updated_by
        FROM proposal_section_requirements m
        WHERE m.tenant_id = NEW.tenant_id AND m.requirement_id = NEW.id
          AND s.tenant_id = m.tenant_id AND s.id = m.section_id
          AND (s.status = 'approved' OR s.approved_rev_no IS NOT NULL);
    UPDATE compliance_matrix SET verify_status = 'stale', stale_reason = format('requirement %s changed', NEW.code), updated_by = NEW.updated_by
        WHERE tenant_id = NEW.tenant_id AND requirement_item_id = NEW.id AND verify_status <> 'stale';
    UPDATE consistency_findings SET status = 'stale', stale_reason = format('requirement %s changed', NEW.code), version = version + 1, updated_by = NEW.updated_by
        WHERE tenant_id = NEW.tenant_id AND requirement_item_id = NEW.id AND status <> 'stale';
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_compliance_requirement_changed ON requirement_items;
CREATE TRIGGER trg_compliance_requirement_changed AFTER UPDATE ON requirement_items
    FOR EACH ROW EXECUTE FUNCTION compliance_requirement_changed();

-- 失效路径 2：章节产生新的有效修订（current_rev_no 变化；候选稿不推进 current_rev_no，不触发）。
CREATE OR REPLACE FUNCTION compliance_section_revised() RETURNS trigger AS $$
DECLARE why TEXT := format('section %s revised: rev %s -> %s', NEW.section_code, OLD.current_rev_no, NEW.current_rev_no);
BEGIN
    UPDATE compliance_matrix SET verify_status = 'stale', stale_reason = why, updated_by = NEW.updated_by
        WHERE tenant_id = NEW.tenant_id AND section_id = NEW.id AND verify_status <> 'stale';
    UPDATE consistency_runs SET status = 'stale', stale_reason = why, updated_by = NEW.updated_by
        WHERE tenant_id = NEW.tenant_id AND outline_id = NEW.outline_id AND status = 'completed'
          AND (snapshot ->> NEW.id::text) IS DISTINCT FROM NEW.current_rev_no::text;
    UPDATE consistency_findings SET status = 'stale', stale_reason = why, version = version + 1, updated_by = NEW.updated_by
        WHERE tenant_id = NEW.tenant_id AND section_ids @> ARRAY[NEW.id] AND status <> 'stale';
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_proposal_sections_compliance ON proposal_sections;
CREATE TRIGGER trg_proposal_sections_compliance AFTER UPDATE OF current_rev_no ON proposal_sections
    FOR EACH ROW WHEN (NEW.current_rev_no IS DISTINCT FROM OLD.current_rev_no)
    EXECUTE FUNCTION compliance_section_revised();

-- 失效路径 3：Phase 4.3 证据变为 stale -> 引用它的矩阵行 stale（名字排在 trg_proposal_evidence_invalidated 之后：
-- 先锁章节再锁矩阵行，与核验事务「章节 -> 矩阵行」的顺序一致，避免死锁）。
CREATE OR REPLACE FUNCTION compliance_evidence_stale() RETURNS trigger AS $$
BEGIN
    IF NEW.status <> 'stale' OR OLD.status = 'stale' THEN
        RETURN NULL;
    END IF;
    UPDATE compliance_matrix SET verify_status = 'stale', stale_reason = format('evidence %s stale: %s', NEW.id, NEW.reason), updated_by = 'system'
        WHERE tenant_id = NEW.tenant_id AND evidence_ids @> ARRAY[NEW.id] AND verify_status <> 'stale';
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_proposal_evidence_matrix_stale ON retrieval_evidence;
CREATE TRIGGER trg_proposal_evidence_matrix_stale AFTER UPDATE OF status ON retrieval_evidence
    FOR EACH ROW EXECUTE FUNCTION compliance_evidence_stale();

-- 失效路径 4：大纲被取代 -> 该大纲下的矩阵行、一致性运行与其结论 stale。
CREATE OR REPLACE FUNCTION compliance_outline_superseded() RETURNS trigger AS $$
DECLARE why TEXT := format('outline v%s superseded', NEW.version);
BEGIN
    UPDATE compliance_matrix SET verify_status = 'stale', stale_reason = why, updated_by = NEW.updated_by
        WHERE tenant_id = NEW.tenant_id AND outline_id = NEW.id AND verify_status <> 'stale';
    UPDATE consistency_runs SET status = 'stale', stale_reason = why, updated_by = NEW.updated_by
        WHERE tenant_id = NEW.tenant_id AND outline_id = NEW.id AND status <> 'stale';
    UPDATE consistency_findings f SET status = 'stale', stale_reason = why, version = f.version + 1, updated_by = NEW.updated_by
        FROM consistency_runs r
        WHERE r.tenant_id = NEW.tenant_id AND r.outline_id = NEW.id AND f.tenant_id = r.tenant_id AND f.run_id = r.id AND f.status <> 'stale';
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_proposal_outline_compliance_stale ON proposal_outlines;
CREATE TRIGGER trg_proposal_outline_compliance_stale AFTER UPDATE OF status ON proposal_outlines
    FOR EACH ROW WHEN (NEW.status = 'superseded' AND OLD.status <> 'superseded')
    EXECUTE FUNCTION compliance_outline_superseded();

-- ---------------------------------------------------------------------------
-- Phase 6B：标书图表（标书正文域）与锁定版本导出（导出附件域）。
--   proposal_charts   绑定到具体章节修订（section_rev_id + rev_no，复合外键）。章节产生新的有效修订后，绑定旧修订的图表
--                     在查询时判定为过期（rev_no <> 章节 current_rev_no），导出快照只收录与锁定修订一致的图表。
--   export_templates  上传的 .docx 套版模板（文件存导出目录，按租户隔离；内置默认模板不入库）。
--   export_jobs       导出任务：门禁通过时在同一事务内固化 rev_snapshot（section_id -> rev_no）、图表快照与偏离表快照，
--                     快照列不可修改；lease_token 是围栏令牌，过期的 running 任务被回收重试，旧执行者的完成写入一律被拒绝。
-- 均不写入 assets 或 explorations/findings。
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS proposal_charts (
    id             BIGSERIAL PRIMARY KEY,
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    task_id        BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    section_id     BIGINT NOT NULL,
    section_rev_id BIGINT NOT NULL,
    rev_no         INT NOT NULL CHECK (rev_no > 0),
    chart_type     TEXT NOT NULL CHECK (chart_type IN ('architecture','gantt','org','equipment_table')),
    title          TEXT NOT NULL CHECK (title <> ''),
    spec           JSONB NOT NULL DEFAULT '{}',
    source_format  TEXT NOT NULL CHECK (source_format IN ('mermaid','json')),
    source         TEXT NOT NULL CHECK (source <> ''),
    source_hash    TEXT NOT NULL,
    render_status  TEXT NOT NULL DEFAULT 'pending' CHECK (render_status IN ('pending','rendered','unrendered','failed')),
    render_format  TEXT NOT NULL DEFAULT '' CHECK (render_format IN ('','png','native')),
    render_error   TEXT NOT NULL DEFAULT '',
    artifact_path  TEXT NOT NULL DEFAULT '',
    artifact_hash  TEXT NOT NULL DEFAULT '',
    artifact_size  BIGINT NOT NULL DEFAULT 0 CHECK (artifact_size >= 0),
    width_px       INT NOT NULL DEFAULT 0 CHECK (width_px >= 0),
    height_px      INT NOT NULL DEFAULT 0 CHECK (height_px >= 0),
    rendered_at    TIMESTAMPTZ,
    created_by     TEXT NOT NULL DEFAULT '',
    updated_by     TEXT NOT NULL DEFAULT '',
    approved_by    TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_proposal_charts_task_id UNIQUE (tenant_id, task_id, id),
    CONSTRAINT ck_proposal_charts_source CHECK ((chart_type = 'equipment_table') = (source_format = 'json')),
    CONSTRAINT ck_proposal_charts_rendered CHECK (render_status <> 'rendered' OR render_format <> ''),
    CONSTRAINT fk_proposal_charts_section FOREIGN KEY (tenant_id, task_id, section_id)
        REFERENCES proposal_sections(tenant_id, task_id, id) ON DELETE CASCADE,
    CONSTRAINT fk_proposal_charts_revision FOREIGN KEY (tenant_id, section_id, section_rev_id, rev_no)
        REFERENCES section_revisions(tenant_id, section_id, id, rev_no) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_proposal_charts_section ON proposal_charts(tenant_id, section_id, id);
CREATE INDEX IF NOT EXISTS idx_proposal_charts_task ON proposal_charts(tenant_id, task_id, id);
DROP TRIGGER IF EXISTS trg_proposal_charts_upd ON proposal_charts;
CREATE TRIGGER trg_proposal_charts_upd BEFORE UPDATE ON proposal_charts
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE IF NOT EXISTS export_templates (
    id          BIGSERIAL PRIMARY KEY,
    tenant_id   TEXT NOT NULL DEFAULT 'default',
    name        TEXT NOT NULL CHECK (name <> ''),
    file_name   TEXT NOT NULL DEFAULT '',
    path        TEXT NOT NULL CHECK (path <> ''),
    sha256      TEXT NOT NULL,
    size        BIGINT NOT NULL CHECK (size > 0),
    meta        JSONB NOT NULL DEFAULT '{}',
    created_by  TEXT NOT NULL DEFAULT '',
    updated_by  TEXT NOT NULL DEFAULT '',
    approved_by TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_export_templates_hash UNIQUE (tenant_id, sha256),
    CONSTRAINT uq_export_templates_tenant_id UNIQUE (tenant_id, id)
);
DROP TRIGGER IF EXISTS trg_export_templates_upd ON export_templates;
CREATE TRIGGER trg_export_templates_upd BEFORE UPDATE ON export_templates
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE IF NOT EXISTS export_jobs (
    id              BIGSERIAL PRIMARY KEY,
    tenant_id       TEXT NOT NULL DEFAULT 'default',
    task_id         BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    outline_id      BIGINT NOT NULL,
    outline_version INT NOT NULL CHECK (outline_version > 0),
    format          TEXT NOT NULL CHECK (format IN ('docx','pdf')),
    status          TEXT NOT NULL DEFAULT 'queued' CHECK (status IN ('queued','running','succeeded','failed')),
    template_id     BIGINT,
    template_key    TEXT NOT NULL DEFAULT 'default',
    template_hash   TEXT NOT NULL DEFAULT '',
    rev_snapshot    JSONB NOT NULL,
    chart_snapshot  JSONB NOT NULL DEFAULT '[]',
    matrix_snapshot JSONB NOT NULL DEFAULT '[]',
    doc_snapshot    JSONB NOT NULL DEFAULT '{}',
    gate            JSONB NOT NULL DEFAULT '{}',
    artifact_path   TEXT NOT NULL DEFAULT '',
    artifact_name   TEXT NOT NULL DEFAULT '',
    artifact_hash   TEXT NOT NULL DEFAULT '',
    artifact_size   BIGINT NOT NULL DEFAULT 0 CHECK (artifact_size >= 0),
    toc_refreshed   BOOLEAN NOT NULL DEFAULT false,
    warnings        JSONB NOT NULL DEFAULT '[]',
    error           TEXT NOT NULL DEFAULT '',
    attempts        INT NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    max_attempts    INT NOT NULL DEFAULT 3 CHECK (max_attempts > 0),
    lease_owner     TEXT NOT NULL DEFAULT '',
    lease_token     BIGINT NOT NULL DEFAULT 0,
    lease_until     TIMESTAMPTZ,
    started_at      TIMESTAMPTZ,
    finished_at     TIMESTAMPTZ,
    created_by      TEXT NOT NULL DEFAULT '',
    updated_by      TEXT NOT NULL DEFAULT '',
    approved_by     TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_export_jobs_task_id UNIQUE (tenant_id, task_id, id),
    CONSTRAINT ck_export_jobs_artifact CHECK (status <> 'succeeded' OR (artifact_path <> '' AND artifact_hash <> '')),
    CONSTRAINT ck_export_jobs_lease CHECK (status <> 'running' OR lease_until IS NOT NULL),
    CONSTRAINT fk_export_jobs_outline FOREIGN KEY (tenant_id, task_id, outline_id)
        REFERENCES proposal_outlines(tenant_id, task_id, id) ON DELETE CASCADE,
    CONSTRAINT fk_export_jobs_template FOREIGN KEY (tenant_id, template_id)
        REFERENCES export_templates(tenant_id, id) ON DELETE RESTRICT
);
CREATE INDEX IF NOT EXISTS idx_export_jobs_task ON export_jobs(tenant_id, task_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_export_jobs_queue ON export_jobs(status, lease_until) WHERE status IN ('queued','running');
DROP TRIGGER IF EXISTS trg_export_jobs_upd ON export_jobs;
CREATE TRIGGER trg_export_jobs_upd BEFORE UPDATE ON export_jobs
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- 导出快照不可篡改：导出后再编辑章节、重绘图表或重新核验矩阵都不影响已创建任务的快照。
CREATE OR REPLACE FUNCTION export_jobs_immutable() RETURNS trigger AS $$
BEGIN
    IF (NEW.tenant_id, NEW.task_id, NEW.outline_id, NEW.outline_version, NEW.format, NEW.template_id, NEW.template_key, NEW.template_hash,
        NEW.rev_snapshot, NEW.chart_snapshot, NEW.matrix_snapshot, NEW.doc_snapshot, NEW.gate, NEW.created_by, NEW.created_at)
       IS DISTINCT FROM
       (OLD.tenant_id, OLD.task_id, OLD.outline_id, OLD.outline_version, OLD.format, OLD.template_id, OLD.template_key, OLD.template_hash,
        OLD.rev_snapshot, OLD.chart_snapshot, OLD.matrix_snapshot, OLD.doc_snapshot, OLD.gate, OLD.created_by, OLD.created_at) THEN
        RAISE EXCEPTION 'export job % snapshot is immutable', OLD.id USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_export_jobs_immutable ON export_jobs;
CREATE TRIGGER trg_export_jobs_immutable BEFORE UPDATE ON export_jobs
    FOR EACH ROW EXECUTE FUNCTION export_jobs_immutable();

-- Model settings (M1). Stored credentials are AES-GCM ciphertext, never plaintext.
ALTER TABLE llm_profiles
    ADD COLUMN IF NOT EXISTS tenant_id TEXT NOT NULL DEFAULT 'default',
    ADD COLUMN IF NOT EXISTS created_by TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS updated_by TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS approved_by TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS is_default BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS api_key_ciphertext TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS api_key_hint TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS options JSONB NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE llm_profiles DROP CONSTRAINT IF EXISTS llm_profiles_name_key;
CREATE UNIQUE INDEX IF NOT EXISTS idx_llm_profiles_tenant_name ON llm_profiles(tenant_id,name);
CREATE UNIQUE INDEX IF NOT EXISTS idx_llm_profiles_default ON llm_profiles(tenant_id) WHERE is_default;
CREATE TABLE IF NOT EXISTS llm_settings (
    tenant_id TEXT PRIMARY KEY DEFAULT 'default',
    pool_enabled BOOLEAN NOT NULL DEFAULT false,
    bind_fallback BOOLEAN NOT NULL DEFAULT false,
    record_bodies BOOLEAN NOT NULL DEFAULT false,
    retry_policy JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_by TEXT NOT NULL DEFAULT '',
    updated_by TEXT NOT NULL DEFAULT '',
    approved_by TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
ALTER TABLE llm_settings ADD COLUMN IF NOT EXISTS record_bodies BOOLEAN NOT NULL DEFAULT false;
DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM llm_settings WHERE tenant_id='default') THEN
        INSERT INTO llm_settings(tenant_id,pool_enabled)
            VALUES ('default', EXISTS (SELECT 1 FROM llm_profiles WHERE enabled AND in_global_pool));
        UPDATE llm_profiles SET is_default=true WHERE id=(
            SELECT id FROM llm_profiles WHERE enabled AND in_global_pool AND tenant_id='default'
            ORDER BY rank,priority DESC,id LIMIT 1);
    END IF;
END $$;

-- ===========================================================================
-- D2（域 C + 企业证据域）：证据可采性、客观分、承诺台账与最小报价门禁。
--   企业证据是租户级全局对象（不随任务删除）；可采性运行 / 明细 / 绑定 / 台账 / 报价属于任务域 C，
--   随任务级联。旧 company_credentials 数据归 default 租户、subject_type=unknown，不伪装为投标人。
-- ===========================================================================

-- 企业证据主表扩展：主体、证书编号 / 签发机构 / 签发日 / 认证范围、软删除与操作人字段。
ALTER TABLE company_credentials
    ADD COLUMN IF NOT EXISTS tenant_id      TEXT NOT NULL DEFAULT 'default',
    ADD COLUMN IF NOT EXISTS subject_name   TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS subject_type   TEXT NOT NULL DEFAULT 'unknown'
        CHECK (subject_type IN ('unknown','bidder','parent','subsidiary','person','manufacturer','other')),
    ADD COLUMN IF NOT EXISTS certificate_no TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS issuer         TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS issued_on      DATE,
    ADD COLUMN IF NOT EXISTS scope          TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS deleted_at     TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS created_by     TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS updated_by     TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS approved_by    TEXT NOT NULL DEFAULT '';
DROP INDEX IF EXISTS uq_company_credentials_code;
CREATE UNIQUE INDEX IF NOT EXISTS uq_company_credentials_tenant_code
    ON company_credentials(tenant_id, category, code) WHERE code <> '';
CREATE UNIQUE INDEX IF NOT EXISTS uq_company_credentials_tenant_id ON company_credentials(tenant_id, id);
CREATE INDEX IF NOT EXISTS idx_company_credentials_tenant_category
    ON company_credentials(tenant_id, category, id) WHERE deleted_at IS NULL;

-- 业绩独立主表：合同 / 验收日期、金额、业绩主体与子公司标记、项目范围。
CREATE TABLE IF NOT EXISTS company_performance (
    id             BIGSERIAL PRIMARY KEY,
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    code           TEXT NOT NULL CHECK (code <> ''),
    project_name   TEXT NOT NULL CHECK (project_name <> ''),
    buyer          TEXT NOT NULL DEFAULT '',
    contract_no    TEXT NOT NULL DEFAULT '',
    contract_date  DATE,
    acceptance_date DATE,
    amount         NUMERIC(20,4),
    currency       TEXT NOT NULL DEFAULT 'CNY',
    subject_name   TEXT NOT NULL DEFAULT '',
    subject_type   TEXT NOT NULL DEFAULT 'unknown'
                     CHECK (subject_type IN ('unknown','bidder','parent','subsidiary','other')),
    is_subsidiary  BOOLEAN NOT NULL DEFAULT false,
    scope          TEXT NOT NULL DEFAULT '',
    attributes     JSONB NOT NULL DEFAULT '{}',
    deleted_at     TIMESTAMPTZ,
    created_by     TEXT NOT NULL DEFAULT '',
    updated_by     TEXT NOT NULL DEFAULT '',
    approved_by    TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_company_performance_code UNIQUE (tenant_id, code),
    CONSTRAINT uq_company_performance_tenant_id UNIQUE (tenant_id, id)
);
CREATE INDEX IF NOT EXISTS idx_company_performance_tenant ON company_performance(tenant_id, id) WHERE deleted_at IS NULL;
DROP TRIGGER IF EXISTS trg_company_performance_upd ON company_performance;
CREATE TRIGGER trg_company_performance_upd BEFORE UPDATE ON company_performance
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- 证据原件与页码：一条证据可关联合同、验收、多段关键页等多份原件；替换时旧记录置 superseded_at。
CREATE TABLE IF NOT EXISTS company_evidence_originals (
    id              BIGSERIAL PRIMARY KEY,
    tenant_id       TEXT NOT NULL DEFAULT 'default',
    owner_domain    TEXT NOT NULL CHECK (owner_domain IN ('credential','performance')),
    owner_id        BIGINT NOT NULL,
    document_kind   TEXT NOT NULL
                      CHECK (document_kind IN ('certificate','contract','acceptance_report','social_security','product_catalog','other')),
    page_label      TEXT NOT NULL DEFAULT '',
    original_path   TEXT NOT NULL CHECK (original_path <> ''),
    original_sha256 TEXT NOT NULL CHECK (original_sha256 <> ''),
    page_start      INT NOT NULL CHECK (page_start > 0),
    page_end        INT NOT NULL CHECK (page_end >= page_start),
    superseded_at   TIMESTAMPTZ,
    created_by      TEXT NOT NULL DEFAULT '',
    updated_by      TEXT NOT NULL DEFAULT '',
    approved_by     TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_company_evidence_originals_tenant_id UNIQUE (tenant_id, id)
);
CREATE INDEX IF NOT EXISTS idx_evidence_originals_owner
    ON company_evidence_originals(tenant_id, owner_domain, owner_id) WHERE superseded_at IS NULL;
DROP TRIGGER IF EXISTS trg_company_evidence_originals_upd ON company_evidence_originals;
CREATE TRIGGER trg_company_evidence_originals_upd BEFORE UPDATE ON company_evidence_originals
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- 要求与企业证据绑定。现有 requirement_items.evidence_ids 仍只表示 retrieval_evidence，不混入多态主键。
CREATE TABLE IF NOT EXISTS requirement_evidence_bindings (
    id              BIGSERIAL PRIMARY KEY,
    tenant_id       TEXT NOT NULL DEFAULT 'default',
    task_id         BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    requirement_id  BIGINT NOT NULL,
    evidence_domain TEXT NOT NULL CHECK (evidence_domain IN ('credential','performance')),
    evidence_id     BIGINT NOT NULL,
    source_hash     TEXT NOT NULL DEFAULT '',
    created_by      TEXT NOT NULL DEFAULT '',
    updated_by      TEXT NOT NULL DEFAULT '',
    approved_by     TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_requirement_evidence_bindings UNIQUE (tenant_id, task_id, requirement_id, evidence_domain, evidence_id),
    CONSTRAINT uq_requirement_evidence_bindings_tenant_id UNIQUE (tenant_id, id),
    CONSTRAINT fk_requirement_evidence_bindings_requirement FOREIGN KEY (tenant_id, task_id, requirement_id)
        REFERENCES requirement_items(tenant_id, task_id, id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_requirement_evidence_bindings_req
    ON requirement_evidence_bindings(tenant_id, task_id, requirement_id);

-- 要求级可采性运行：MinCount 等聚合规则只在本表统计；每个要求最多一个 current 运行。
CREATE TABLE IF NOT EXISTS admissibility_runs (
    id                 BIGSERIAL PRIMARY KEY,
    tenant_id          TEXT NOT NULL DEFAULT 'default',
    task_id            BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    requirement_id     BIGINT NOT NULL,
    rule_version       TEXT NOT NULL CHECK (rule_version <> ''),
    bid_deadline       TIMESTAMPTZ,
    source_fingerprint TEXT NOT NULL CHECK (source_fingerprint <> ''),
    verdict            TEXT NOT NULL CHECK (verdict IN ('admissible','inadmissible','manual')),
    checks             JSONB NOT NULL DEFAULT '[]',
    reasons            JSONB NOT NULL DEFAULT '[]',
    status             TEXT NOT NULL DEFAULT 'current' CHECK (status IN ('current','stale','superseded')),
    evaluated_by       TEXT NOT NULL DEFAULT '',
    evaluated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    version            INT NOT NULL DEFAULT 1 CHECK (version > 0),
    created_by         TEXT NOT NULL DEFAULT '',
    updated_by         TEXT NOT NULL DEFAULT '',
    approved_by        TEXT NOT NULL DEFAULT '',
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_admissibility_runs_tenant_id UNIQUE (tenant_id, id),
    CONSTRAINT fk_admissibility_runs_requirement FOREIGN KEY (tenant_id, task_id, requirement_id)
        REFERENCES requirement_items(tenant_id, task_id, id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_admissibility_runs_current
    ON admissibility_runs(tenant_id, task_id, requirement_id) WHERE status = 'current';
CREATE INDEX IF NOT EXISTS idx_admissibility_runs_task ON admissibility_runs(tenant_id, task_id, requirement_id, version DESC);
DROP TRIGGER IF EXISTS trg_admissibility_runs_upd ON admissibility_runs;
CREATE TRIGGER trg_admissibility_runs_upd BEFORE UPDATE ON admissibility_runs
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- 运行内逐证据结论：保存主体、原件、有效期、日期窗口、合同/验收、型号等单证据规则结果。
CREATE TABLE IF NOT EXISTS admissibility_items (
    id         BIGSERIAL PRIMARY KEY,
    tenant_id  TEXT NOT NULL DEFAULT 'default',
    run_id     BIGINT NOT NULL,
    binding_id BIGINT NOT NULL,
    verdict    TEXT NOT NULL CHECK (verdict IN ('admissible','inadmissible','manual')),
    checks     JSONB NOT NULL DEFAULT '[]',
    reasons    JSONB NOT NULL DEFAULT '[]',
    created_by TEXT NOT NULL DEFAULT '',
    updated_by TEXT NOT NULL DEFAULT '',
    approved_by TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_admissibility_items UNIQUE (tenant_id, run_id, binding_id),
    CONSTRAINT uq_admissibility_items_tenant_id UNIQUE (tenant_id, id),
    CONSTRAINT fk_admissibility_items_run FOREIGN KEY (tenant_id, run_id)
        REFERENCES admissibility_runs(tenant_id, id) ON DELETE CASCADE,
    CONSTRAINT fk_admissibility_items_binding FOREIGN KEY (tenant_id, binding_id)
        REFERENCES requirement_evidence_bindings(tenant_id, id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_admissibility_items_run ON admissibility_items(tenant_id, run_id);
DROP TRIGGER IF EXISTS trg_admissibility_items_upd ON admissibility_items;
CREATE TRIGGER trg_admissibility_items_upd BEFORE UPDATE ON admissibility_items
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- 客观分审计快照：只统计当前运行中 admissible 的证据；规则无法解析时 status=manual 并留缺口。
CREATE TABLE IF NOT EXISTS objective_score_runs (
    id                   BIGSERIAL PRIMARY KEY,
    tenant_id            TEXT NOT NULL DEFAULT 'default',
    task_id              BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    requirement_id       BIGINT NOT NULL,
    admissibility_run_id BIGINT,
    rule_version         TEXT NOT NULL CHECK (rule_version <> ''),
    status               TEXT NOT NULL CHECK (status IN ('computed','manual')),
    score                NUMERIC(10,4) NOT NULL DEFAULT 0,
    max_score            NUMERIC(10,4) NOT NULL DEFAULT 0,
    evidence_ids         BIGINT[] NOT NULL DEFAULT '{}',
    gaps                 JSONB NOT NULL DEFAULT '[]',
    computed_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by           TEXT NOT NULL DEFAULT '',
    updated_by           TEXT NOT NULL DEFAULT '',
    approved_by          TEXT NOT NULL DEFAULT '',
    CONSTRAINT uq_objective_score_runs_tenant_id UNIQUE (tenant_id, id),
    CONSTRAINT fk_objective_score_runs_requirement FOREIGN KEY (tenant_id, task_id, requirement_id)
        REFERENCES requirement_items(tenant_id, task_id, id) ON DELETE CASCADE,
    CONSTRAINT fk_objective_score_runs_admissibility FOREIGN KEY (tenant_id, admissibility_run_id)
        REFERENCES admissibility_runs(tenant_id, id) ON DELETE SET NULL
);
CREATE INDEX IF NOT EXISTS idx_objective_score_runs_task ON objective_score_runs(tenant_id, task_id, requirement_id, id DESC);

-- 承诺台账：指标、招标阈值原文/归一值、承诺原文/归一值、能力上限、责任人、依据、状态、版本与会签。
CREATE TABLE IF NOT EXISTS commitment_ledger (
    id                            BIGSERIAL PRIMARY KEY,
    tenant_id                     TEXT NOT NULL DEFAULT 'default',
    task_id                       BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    code                          TEXT NOT NULL CHECK (code <> ''),
    requirement_id                BIGINT,
    metric                        TEXT NOT NULL
                                    CHECK (metric IN ('duration','warranty','sla_response','sla_onsite','sla_repair','availability','team_size','price','other')),
    metric_label                  TEXT NOT NULL DEFAULT '',
    unit                          TEXT NOT NULL DEFAULT '',
    threshold_cmp                 TEXT NOT NULL DEFAULT '>=' CHECK (threshold_cmp IN ('>=','<=','=')),
    tender_threshold_raw          TEXT NOT NULL DEFAULT '',
    tender_threshold_value        NUMERIC(20,4),
    committed_raw                 TEXT NOT NULL DEFAULT '',
    committed_value               NUMERIC(20,4),
    capability_limit              NUMERIC(20,4),
    capability_basis              TEXT NOT NULL DEFAULT '',
    owner                         TEXT NOT NULL DEFAULT '',
    rationale                     TEXT NOT NULL DEFAULT '',
    source_admissibility_item_ids BIGINT[] NOT NULL DEFAULT '{}',
    status                        TEXT NOT NULL DEFAULT 'draft' CHECK (status IN ('draft','confirmed','signed_off','rejected')),
    requires_delivery_signoff     BOOLEAN NOT NULL DEFAULT false,
    signed_off_by                 TEXT NOT NULL DEFAULT '',
    signed_off_at                 TIMESTAMPTZ,
    version                       INT NOT NULL DEFAULT 1 CHECK (version > 0),
    created_by                    TEXT NOT NULL DEFAULT '',
    updated_by                    TEXT NOT NULL DEFAULT '',
    approved_by                   TEXT NOT NULL DEFAULT '',
    created_at                    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_commitment_ledger_code UNIQUE (tenant_id, task_id, code),
    CONSTRAINT uq_commitment_ledger_tenant_id UNIQUE (tenant_id, id),
    CONSTRAINT fk_commitment_ledger_requirement FOREIGN KEY (tenant_id, task_id, requirement_id)
        REFERENCES requirement_items(tenant_id, task_id, id) ON DELETE SET NULL (requirement_id)
);
CREATE INDEX IF NOT EXISTS idx_commitment_ledger_task ON commitment_ledger(tenant_id, task_id, status, id);
DROP TRIGGER IF EXISTS trg_commitment_ledger_upd ON commitment_ledger;
CREATE TRIGGER trg_commitment_ledger_upd BEFORE UPDATE ON commitment_ledger
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- 最小报价门禁：报价版本与行。金额为定点 NUMERIC，Go 层以 0.0001 为最小单位计算。
CREATE TABLE IF NOT EXISTS bid_quotes (
    id              BIGSERIAL PRIMARY KEY,
    tenant_id       TEXT NOT NULL DEFAULT 'default',
    task_id         BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    lot             TEXT NOT NULL DEFAULT '',
    version         INT NOT NULL CHECK (version > 0),
    declared_total  NUMERIC(20,4) NOT NULL DEFAULT 0,
    ceiling         NUMERIC(20,4),
    currency        TEXT NOT NULL DEFAULT 'CNY',
    status          TEXT NOT NULL DEFAULT 'draft' CHECK (status IN ('draft','confirmed','rejected')),
    original_path   TEXT NOT NULL DEFAULT '',
    original_sha256 TEXT NOT NULL DEFAULT '',
    note            TEXT NOT NULL DEFAULT '',
    confirmed_by    TEXT NOT NULL DEFAULT '',
    confirmed_at    TIMESTAMPTZ,
    created_by      TEXT NOT NULL DEFAULT '',
    updated_by      TEXT NOT NULL DEFAULT '',
    approved_by     TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_bid_quotes_version UNIQUE (tenant_id, task_id, lot, version),
    CONSTRAINT uq_bid_quotes_tenant_id UNIQUE (tenant_id, id)
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_bid_quotes_confirmed ON bid_quotes(tenant_id, task_id, lot) WHERE status = 'confirmed';
DROP TRIGGER IF EXISTS trg_bid_quotes_upd ON bid_quotes;
CREATE TRIGGER trg_bid_quotes_upd BEFORE UPDATE ON bid_quotes
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE IF NOT EXISTS bid_quote_items (
    id             BIGSERIAL PRIMARY KEY,
    tenant_id      TEXT NOT NULL DEFAULT 'default',
    quote_id       BIGINT NOT NULL,
    line_no        INT NOT NULL CHECK (line_no > 0),
    code           TEXT NOT NULL DEFAULT '',
    name           TEXT NOT NULL CHECK (name <> ''),
    unit_price     NUMERIC(20,4) NOT NULL,
    quantity       NUMERIC(20,4) NOT NULL,
    line_amount    NUMERIC(20,4) NOT NULL,
    requirement_id BIGINT,
    model          TEXT NOT NULL DEFAULT '',
    created_by     TEXT NOT NULL DEFAULT '',
    updated_by     TEXT NOT NULL DEFAULT '',
    approved_by    TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_bid_quote_items_line UNIQUE (tenant_id, quote_id, line_no),
    CONSTRAINT fk_bid_quote_items_quote FOREIGN KEY (tenant_id, quote_id)
        REFERENCES bid_quotes(tenant_id, id) ON DELETE CASCADE
);
DROP TRIGGER IF EXISTS trg_bid_quote_items_upd ON bid_quote_items;
CREATE TRIGGER trg_bid_quote_items_upd BEFORE UPDATE ON bid_quote_items
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- 章节修订引用当前可采性明细，而不是多态企业证据 ID。
CREATE UNIQUE INDEX IF NOT EXISTS uq_section_revisions_tenant_task_id ON section_revisions(tenant_id, task_id, id);
CREATE TABLE IF NOT EXISTS section_revision_evidence (
    tenant_id            TEXT NOT NULL DEFAULT 'default',
    task_id              BIGINT NOT NULL,
    section_rev_id       BIGINT NOT NULL,
    admissibility_item_id BIGINT NOT NULL,
    created_by           TEXT NOT NULL DEFAULT '',
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, section_rev_id, admissibility_item_id),
    CONSTRAINT fk_section_revision_evidence_revision FOREIGN KEY (tenant_id, task_id, section_rev_id)
        REFERENCES section_revisions(tenant_id, task_id, id) ON DELETE CASCADE,
    CONSTRAINT fk_section_revision_evidence_item FOREIGN KEY (tenant_id, admissibility_item_id)
        REFERENCES admissibility_items(tenant_id, id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_section_revision_evidence_item ON section_revision_evidence(tenant_id, admissibility_item_id);
ALTER TABLE section_revisions ADD COLUMN IF NOT EXISTS commitment_ids BIGINT[] NOT NULL DEFAULT '{}';
CREATE INDEX IF NOT EXISTS idx_section_revisions_commitments ON section_revisions USING GIN(commitment_ids);
ALTER TABLE section_revisions DROP CONSTRAINT IF EXISTS section_revisions_status_check;
ALTER TABLE section_revisions ADD CONSTRAINT section_revisions_status_check
    CHECK (status IN ('draft','reviewing','approved','rejected','stale_outline','stale_evidence','stale_commitment','conflict_candidate'));
ALTER TABLE proposal_sections DROP CONSTRAINT IF EXISTS proposal_sections_status_check;
ALTER TABLE proposal_sections ADD CONSTRAINT proposal_sections_status_check
    CHECK (status IN ('pending','ready','writing','draft','reviewing','approved','rejected','stale_outline','stale_evidence','stale_commitment'));

-- 修订不可篡改：commitment_ids 也属于不可变内容。
CREATE OR REPLACE FUNCTION section_revisions_immutable() RETURNS trigger AS $$
BEGIN
    IF (NEW.tenant_id, NEW.task_id, NEW.section_id, NEW.rev_no, NEW.base_rev_no, NEW.based_on_outline_version, NEW.source,
        NEW.rollback_of_rev_no, NEW.content_md, NEW.summary_note, NEW.diff_summary, NEW.evidence_ids, NEW.commitment_ids, NEW.gaps, NEW.created_by, NEW.created_at)
       IS DISTINCT FROM
       (OLD.tenant_id, OLD.task_id, OLD.section_id, OLD.rev_no, OLD.base_rev_no, OLD.based_on_outline_version, OLD.source,
        OLD.rollback_of_rev_no, OLD.content_md, OLD.summary_note, OLD.diff_summary, OLD.evidence_ids, OLD.commitment_ids, OLD.gaps, OLD.created_by, OLD.created_at) THEN
        RAISE EXCEPTION 'section revision % is immutable', OLD.id USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- ---------------------------------------------------------------------------
-- D2 失效传播：证据 / 原件 / 绑定 / 要求 / 截止日变化 -> 当前可采性运行 stale；
-- 运行 stale/superseded -> 引用其明细的章节降级、审批撤销、矩阵与一致性 stale。
-- 台账变更 -> 引用它的章节降级、矩阵与一致性 stale。
-- ---------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION d2_mark_runs_stale(t TEXT, dom TEXT, eid BIGINT, why TEXT) RETURNS void AS $$
BEGIN
    UPDATE admissibility_runs r
       SET status = 'stale',
           reasons = COALESCE(r.reasons, '[]'::jsonb) || jsonb_build_array(jsonb_build_object('code','source_changed','message',why)),
           updated_by = 'system'
     WHERE r.tenant_id = t AND r.status = 'current'
       AND EXISTS (SELECT 1 FROM requirement_evidence_bindings b
                    WHERE b.tenant_id = t AND b.task_id = r.task_id AND b.requirement_id = r.requirement_id
                      AND b.evidence_domain = dom AND b.evidence_id = eid);
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION d2_credential_changed() RETURNS trigger AS $$
DECLARE t TEXT; eid BIGINT;
BEGIN
    IF TG_OP = 'DELETE' THEN
        t := OLD.tenant_id; eid := OLD.id;
    ELSE
        t := NEW.tenant_id; eid := NEW.id;
        IF TG_OP = 'UPDATE' AND
           (NEW.category, NEW.code, NEW.title, NEW.attributes, NEW.valid_until, NEW.subject_name, NEW.subject_type,
            NEW.certificate_no, NEW.issuer, NEW.issued_on, NEW.scope, NEW.deleted_at)
           IS NOT DISTINCT FROM
           (OLD.category, OLD.code, OLD.title, OLD.attributes, OLD.valid_until, OLD.subject_name, OLD.subject_type,
            OLD.certificate_no, OLD.issuer, OLD.issued_on, OLD.scope, OLD.deleted_at) THEN
            RETURN NULL;
        END IF;
    END IF;
    PERFORM d2_mark_runs_stale(t, 'credential', eid, 'credential source changed or deleted');
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_d2_credential_changed ON company_credentials;
CREATE TRIGGER trg_d2_credential_changed AFTER INSERT OR UPDATE OR DELETE ON company_credentials
    FOR EACH ROW EXECUTE FUNCTION d2_credential_changed();

CREATE OR REPLACE FUNCTION d2_performance_changed() RETURNS trigger AS $$
DECLARE t TEXT; eid BIGINT;
BEGIN
    IF TG_OP = 'DELETE' THEN
        t := OLD.tenant_id; eid := OLD.id;
    ELSE
        t := NEW.tenant_id; eid := NEW.id;
        IF TG_OP = 'UPDATE' AND
           (NEW.project_name, NEW.buyer, NEW.contract_no, NEW.contract_date, NEW.acceptance_date, NEW.amount, NEW.currency,
            NEW.subject_name, NEW.subject_type, NEW.is_subsidiary, NEW.scope, NEW.attributes, NEW.deleted_at)
           IS NOT DISTINCT FROM
           (OLD.project_name, OLD.buyer, OLD.contract_no, OLD.contract_date, OLD.acceptance_date, OLD.amount, OLD.currency,
            OLD.subject_name, OLD.subject_type, OLD.is_subsidiary, OLD.scope, OLD.attributes, OLD.deleted_at) THEN
            RETURN NULL;
        END IF;
    END IF;
    PERFORM d2_mark_runs_stale(t, 'performance', eid, 'performance source changed or deleted');
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_d2_performance_changed ON company_performance;
CREATE TRIGGER trg_d2_performance_changed AFTER INSERT OR UPDATE OR DELETE ON company_performance
    FOR EACH ROW EXECUTE FUNCTION d2_performance_changed();

CREATE OR REPLACE FUNCTION d2_original_changed() RETURNS trigger AS $$
DECLARE t TEXT; dom TEXT; eid BIGINT;
BEGIN
    IF TG_OP = 'DELETE' THEN
        t := OLD.tenant_id; dom := OLD.owner_domain; eid := OLD.owner_id;
    ELSE
        t := NEW.tenant_id; dom := NEW.owner_domain; eid := NEW.owner_id;
        IF TG_OP = 'UPDATE' AND
           (NEW.owner_domain, NEW.owner_id, NEW.document_kind, NEW.page_label, NEW.original_path, NEW.original_sha256,
            NEW.page_start, NEW.page_end, NEW.superseded_at)
           IS NOT DISTINCT FROM
           (OLD.owner_domain, OLD.owner_id, OLD.document_kind, OLD.page_label, OLD.original_path, OLD.original_sha256,
            OLD.page_start, OLD.page_end, OLD.superseded_at) THEN
            RETURN NULL;
        END IF;
    END IF;
    PERFORM d2_mark_runs_stale(t, dom, eid, 'evidence original changed or replaced');
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_d2_original_changed ON company_evidence_originals;
CREATE TRIGGER trg_d2_original_changed AFTER INSERT OR UPDATE OR DELETE ON company_evidence_originals
    FOR EACH ROW EXECUTE FUNCTION d2_original_changed();

CREATE OR REPLACE FUNCTION d2_binding_changed() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        UPDATE admissibility_runs SET status = 'stale', updated_by = 'system'
            WHERE tenant_id = OLD.tenant_id AND task_id = OLD.task_id AND requirement_id = OLD.requirement_id AND status = 'current';
    ELSE
        UPDATE admissibility_runs SET status = 'stale', updated_by = 'system'
            WHERE tenant_id = NEW.tenant_id AND task_id = NEW.task_id AND requirement_id = NEW.requirement_id AND status = 'current';
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_d2_binding_changed ON requirement_evidence_bindings;
CREATE TRIGGER trg_d2_binding_changed AFTER INSERT OR DELETE ON requirement_evidence_bindings
    FOR EACH ROW EXECUTE FUNCTION d2_binding_changed();

CREATE OR REPLACE FUNCTION d2_requirement_changed() RETURNS trigger AS $$
BEGIN
    IF (NEW.code, NEW.category, NEW.title, NEW.description, NEW.is_mandatory, NEW.score_weight, NEW.score_group,
        NEW.source_hash, NEW.proof_rules, NEW.scoring_tiers, NEW.eval_kind, NEW.parent_id, NEW.superseded_by_id, NEW.review_status)
       IS NOT DISTINCT FROM
       (OLD.code, OLD.category, OLD.title, OLD.description, OLD.is_mandatory, OLD.score_weight, OLD.score_group,
        OLD.source_hash, OLD.proof_rules, OLD.scoring_tiers, OLD.eval_kind, OLD.parent_id, OLD.superseded_by_id, OLD.review_status) THEN
        RETURN NULL;
    END IF;
    UPDATE admissibility_runs SET status = 'stale', updated_by = 'system'
        WHERE tenant_id = NEW.tenant_id AND task_id = NEW.task_id AND requirement_id = NEW.id AND status = 'current';
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_d2_requirement_changed ON requirement_items;
CREATE TRIGGER trg_d2_requirement_changed AFTER UPDATE ON requirement_items
    FOR EACH ROW EXECUTE FUNCTION d2_requirement_changed();

CREATE OR REPLACE FUNCTION d2_deadline_changed() RETURNS trigger AS $$
BEGIN
    IF NEW.bid_deadline IS NOT DISTINCT FROM OLD.bid_deadline THEN
        RETURN NULL;
    END IF;
    UPDATE admissibility_runs SET status = 'stale', updated_by = 'system'
        WHERE tenant_id = NEW.tenant_id AND task_id = NEW.task_id AND status = 'current';
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_d2_deadline_changed ON requirement_manifests;
CREATE TRIGGER trg_d2_deadline_changed AFTER UPDATE OF bid_deadline ON requirement_manifests
    FOR EACH ROW EXECUTE FUNCTION d2_deadline_changed();

CREATE OR REPLACE FUNCTION d2_run_invalidated() RETURNS trigger AS $$
DECLARE
    why TEXT;
    affected BIGINT[];
BEGIN
    IF NEW.status NOT IN ('stale','superseded') OR OLD.status <> 'current' THEN
        RETURN NULL;
    END IF;
    why := format('admissibility run %s is %s', NEW.id, NEW.status);
    SELECT COALESCE(array_agg(DISTINCT rv.section_id), '{}') INTO affected
      FROM section_revision_evidence sre
      JOIN admissibility_items i ON i.tenant_id = sre.tenant_id AND i.id = sre.admissibility_item_id
      JOIN section_revisions rv ON rv.tenant_id = sre.tenant_id AND rv.id = sre.section_rev_id
     WHERE i.tenant_id = NEW.tenant_id AND i.run_id = NEW.id;
    UPDATE section_revisions rv SET status = 'stale_evidence', updated_by = 'system'
        WHERE rv.tenant_id = NEW.tenant_id AND rv.status IN ('draft','reviewing','approved')
          AND EXISTS (SELECT 1 FROM section_revision_evidence sre
                      JOIN admissibility_items i ON i.tenant_id = sre.tenant_id AND i.id = sre.admissibility_item_id
                      WHERE sre.tenant_id = rv.tenant_id AND sre.section_rev_id = rv.id AND i.run_id = NEW.id);
    UPDATE proposal_sections s SET status = 'stale_evidence', approved_rev_no = NULL, updated_by = 'system'
        WHERE s.tenant_id = NEW.tenant_id AND s.status <> 'stale_outline' AND s.approved_rev_no IS NOT NULL
          AND s.id = ANY(affected);
    UPDATE compliance_matrix SET verify_status = 'stale', stale_reason = why, updated_by = 'system'
        WHERE tenant_id = NEW.tenant_id AND section_id = ANY(affected) AND verify_status <> 'stale';
    UPDATE consistency_runs SET status = 'stale', stale_reason = why, updated_by = 'system'
        WHERE tenant_id = NEW.tenant_id AND task_id = NEW.task_id AND status = 'completed';
    UPDATE consistency_findings SET status = 'stale', stale_reason = why, version = version + 1, updated_by = 'system'
        WHERE tenant_id = NEW.tenant_id AND task_id = NEW.task_id AND section_ids && affected AND status <> 'stale';
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_d2_run_invalidated ON admissibility_runs;
CREATE TRIGGER trg_d2_run_invalidated AFTER UPDATE OF status ON admissibility_runs
    FOR EACH ROW EXECUTE FUNCTION d2_run_invalidated();

CREATE OR REPLACE FUNCTION d2_commitment_changed() RETURNS trigger AS $$
DECLARE
    t TEXT; tid BIGINT; cid BIGINT; why TEXT; affected BIGINT[];
BEGIN
    IF TG_OP = 'DELETE' THEN
        t := OLD.tenant_id; tid := OLD.task_id; cid := OLD.id;
    ELSE
        t := NEW.tenant_id; tid := NEW.task_id; cid := NEW.id;
        IF TG_OP = 'UPDATE' AND
           (NEW.committed_value, NEW.tender_threshold_value, NEW.threshold_cmp, NEW.status, NEW.capability_limit,
            NEW.capability_basis, NEW.source_admissibility_item_ids, NEW.version, NEW.unit, NEW.metric)
           IS NOT DISTINCT FROM
           (OLD.committed_value, OLD.tender_threshold_value, OLD.threshold_cmp, OLD.status, OLD.capability_limit,
            OLD.capability_basis, OLD.source_admissibility_item_ids, OLD.version, OLD.unit, OLD.metric) THEN
            RETURN NULL;
        END IF;
    END IF;
    why := format('commitment %s changed', cid);
    SELECT COALESCE(array_agg(DISTINCT rv.section_id), '{}') INTO affected
      FROM section_revisions rv
     WHERE rv.tenant_id = t AND rv.commitment_ids @> ARRAY[cid];
    UPDATE section_revisions rv SET status = 'stale_commitment', updated_by = 'system'
        WHERE rv.tenant_id = t AND rv.status IN ('draft','reviewing','approved') AND rv.commitment_ids @> ARRAY[cid];
    UPDATE proposal_sections s SET status = 'stale_commitment', approved_rev_no = NULL, updated_by = 'system'
        WHERE s.tenant_id = t AND s.status <> 'stale_outline' AND s.approved_rev_no IS NOT NULL AND s.id = ANY(affected);
    UPDATE compliance_matrix SET verify_status = 'stale', stale_reason = why, updated_by = 'system'
        WHERE tenant_id = t AND section_id = ANY(affected) AND verify_status <> 'stale';
    UPDATE consistency_runs SET status = 'stale', stale_reason = why, updated_by = 'system'
        WHERE tenant_id = t AND task_id = tid AND status = 'completed';
    UPDATE consistency_findings SET status = 'stale', stale_reason = why, version = version + 1, updated_by = 'system'
        WHERE tenant_id = t AND section_ids && affected AND status <> 'stale';
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_d2_commitment_changed ON commitment_ledger;
CREATE TRIGGER trg_d2_commitment_changed AFTER UPDATE OR DELETE ON commitment_ledger
    FOR EACH ROW EXECUTE FUNCTION d2_commitment_changed();

-- 已绑定证据不得硬删除；场景清理在任务删除（绑定级联清空）后按租户精确物理删除。
CREATE OR REPLACE FUNCTION d2_guard_evidence_delete() RETURNS trigger AS $$
DECLARE dom TEXT;
BEGIN
    IF TG_TABLE_NAME = 'company_performance' THEN dom := 'performance'; ELSE dom := 'credential'; END IF;
    IF EXISTS (SELECT 1 FROM requirement_evidence_bindings b
                WHERE b.tenant_id = OLD.tenant_id AND b.evidence_domain = dom AND b.evidence_id = OLD.id) THEN
        RAISE EXCEPTION 'evidence % % is bound to a task and cannot be deleted', dom, OLD.id USING ERRCODE = 'check_violation';
    END IF;
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_d2_guard_credential_delete ON company_credentials;
CREATE TRIGGER trg_d2_guard_credential_delete BEFORE DELETE ON company_credentials
    FOR EACH ROW EXECUTE FUNCTION d2_guard_evidence_delete();
DROP TRIGGER IF EXISTS trg_d2_guard_performance_delete ON company_performance;
CREATE TRIGGER trg_d2_guard_performance_delete BEFORE DELETE ON company_performance
    FOR EACH ROW EXECUTE FUNCTION d2_guard_evidence_delete();

-- D2 不可变约束：已确认报价、可采性历史运行 / 明细与原件内容不得原地篡改。
-- 报价作废通过 status='rejected' 的受控状态变更实现；原件替换通过 superseded_at + 新增记录实现。
CREATE OR REPLACE FUNCTION d2_quote_item_immutable() RETURNS trigger AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM bid_quotes q WHERE q.tenant_id = OLD.tenant_id AND q.id = OLD.quote_id AND q.status = 'confirmed') THEN
        RAISE EXCEPTION 'confirmed quote items are immutable' USING ERRCODE = 'check_violation';
    END IF;
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_d2_quote_item_immutable ON bid_quote_items;
CREATE TRIGGER trg_d2_quote_item_immutable BEFORE UPDATE OR DELETE ON bid_quote_items
    FOR EACH ROW EXECUTE FUNCTION d2_quote_item_immutable();

CREATE OR REPLACE FUNCTION d2_quote_immutable() RETURNS trigger AS $$
BEGIN
    IF OLD.status = 'confirmed' AND
       (NEW.declared_total, NEW.ceiling, NEW.currency, NEW.lot, NEW.version)
       IS DISTINCT FROM (OLD.declared_total, OLD.ceiling, OLD.currency, OLD.lot, OLD.version) THEN
        RAISE EXCEPTION 'confirmed quote amounts are immutable' USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_d2_quote_immutable ON bid_quotes;
CREATE TRIGGER trg_d2_quote_immutable BEFORE UPDATE ON bid_quotes
    FOR EACH ROW EXECUTE FUNCTION d2_quote_immutable();

CREATE OR REPLACE FUNCTION d2_admissibility_item_immutable() RETURNS trigger AS $$
BEGIN
    IF (NEW.run_id, NEW.binding_id, NEW.verdict, NEW.checks, NEW.reasons)
       IS DISTINCT FROM (OLD.run_id, OLD.binding_id, OLD.verdict, OLD.checks, OLD.reasons) THEN
        RAISE EXCEPTION 'admissibility item % is immutable', OLD.id USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_d2_admissibility_item_immutable ON admissibility_items;
CREATE TRIGGER trg_d2_admissibility_item_immutable BEFORE UPDATE ON admissibility_items
    FOR EACH ROW EXECUTE FUNCTION d2_admissibility_item_immutable();

CREATE OR REPLACE FUNCTION d2_admissibility_run_immutable() RETURNS trigger AS $$
BEGIN
    IF (NEW.requirement_id, NEW.rule_version, NEW.bid_deadline, NEW.source_fingerprint, NEW.verdict, NEW.checks, NEW.version)
       IS DISTINCT FROM (OLD.requirement_id, OLD.rule_version, OLD.bid_deadline, OLD.source_fingerprint, OLD.verdict, OLD.checks, OLD.version) THEN
        RAISE EXCEPTION 'admissibility run % conclusion is immutable', OLD.id USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_d2_admissibility_run_immutable ON admissibility_runs;
CREATE TRIGGER trg_d2_admissibility_run_immutable BEFORE UPDATE ON admissibility_runs
    FOR EACH ROW EXECUTE FUNCTION d2_admissibility_run_immutable();

CREATE OR REPLACE FUNCTION d2_original_immutable() RETURNS trigger AS $$
BEGIN
    IF (NEW.owner_domain, NEW.owner_id, NEW.document_kind, NEW.page_label, NEW.original_path, NEW.original_sha256,
        NEW.page_start, NEW.page_end)
       IS DISTINCT FROM (OLD.owner_domain, OLD.owner_id, OLD.document_kind, OLD.page_label, OLD.original_path, OLD.original_sha256,
        OLD.page_start, OLD.page_end) THEN
        RAISE EXCEPTION 'evidence original % content is immutable; supersede it and insert a new record', OLD.id USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_d2_original_immutable ON company_evidence_originals;
CREATE TRIGGER trg_d2_original_immutable BEFORE UPDATE ON company_evidence_originals
    FOR EACH ROW EXECUTE FUNCTION d2_original_immutable();

-- 客观分变化：引用该要求的矩阵行、一致性结论与结论 stale；导出门禁读取最新计算。
CREATE OR REPLACE FUNCTION d2_objective_changed() RETURNS trigger AS $$
DECLARE why TEXT := format('objective score for requirement %s recomputed', NEW.requirement_id);
BEGIN
    UPDATE compliance_matrix SET verify_status = 'stale', stale_reason = why, updated_by = 'system'
        WHERE tenant_id = NEW.tenant_id AND requirement_item_id = NEW.requirement_id AND verify_status <> 'stale';
    UPDATE consistency_runs SET status = 'stale', stale_reason = why, updated_by = 'system'
        WHERE tenant_id = NEW.tenant_id AND task_id = NEW.task_id AND status = 'completed';
    UPDATE consistency_findings SET status = 'stale', stale_reason = why, version = version + 1, updated_by = 'system'
        WHERE tenant_id = NEW.tenant_id AND task_id = NEW.task_id AND requirement_item_id = NEW.requirement_id AND status <> 'stale';
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_d2_objective_changed ON objective_score_runs;
CREATE TRIGGER trg_d2_objective_changed AFTER INSERT ON objective_score_runs
    FOR EACH ROW EXECUTE FUNCTION d2_objective_changed();


-- =============================================================================
-- D3：章节故事板（Pink Team 粉队评审）与句子级证据绑定写作核验（Domain C）
-- =============================================================================

CREATE UNIQUE INDEX IF NOT EXISTS uq_proposal_sections_tenant_id ON proposal_sections(tenant_id, id);
CREATE UNIQUE INDEX IF NOT EXISTS uq_section_revisions_tenant_id ON section_revisions(tenant_id, id);

CREATE TABLE IF NOT EXISTS section_storyboards (
    id                      BIGSERIAL PRIMARY KEY,
    tenant_id               TEXT NOT NULL DEFAULT 'default',
    task_id                 BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    section_id              BIGINT NOT NULL,
    outline_version         INT NOT NULL DEFAULT 1,
    version                 INT NOT NULL DEFAULT 1 CHECK (version >= 1),
    theme_sentence          TEXT NOT NULL DEFAULT '',
    scoring_points          JSONB NOT NULL DEFAULT '[]'::jsonb,
    admissibility_item_ids  BIGINT[] NOT NULL DEFAULT '{}',
    evidence_ids            BIGINT[] NOT NULL DEFAULT '{}',
    commitment_ids          BIGINT[] NOT NULL DEFAULT '{}',
    chart_plans             JSONB NOT NULL DEFAULT '[]'::jsonb,
    status                  TEXT NOT NULL DEFAULT 'draft'
                            CHECK (status IN ('draft','pending_pink_review','pink_approved','pink_rejected','stale','superseded')),
    stale_reason            TEXT NOT NULL DEFAULT '',
    review_note             TEXT NOT NULL DEFAULT '',
    submitted_by            TEXT NOT NULL DEFAULT '',
    submitted_at            TIMESTAMPTZ,
    reviewed_by             TEXT NOT NULL DEFAULT '',
    reviewed_at             TIMESTAMPTZ,
    created_by              TEXT NOT NULL DEFAULT 'system',
    updated_by              TEXT NOT NULL DEFAULT 'system',
    approved_by             TEXT NOT NULL DEFAULT '',
    created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (tenant_id, id),
    UNIQUE (tenant_id, section_id, version),
    FOREIGN KEY (tenant_id, section_id) REFERENCES proposal_sections(tenant_id, id) ON DELETE CASCADE,
    CONSTRAINT section_storyboards_pink_separation_chk CHECK (
        status <> 'pink_approved' OR (
            approved_by <> '' AND
            approved_by <> created_by AND
            (submitted_by = '' OR approved_by <> submitted_by)
        )
    )
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_section_storyboards_current
    ON section_storyboards(tenant_id, section_id) WHERE status <> 'superseded';
CREATE INDEX IF NOT EXISTS idx_section_storyboards_task_section
    ON section_storyboards(tenant_id, task_id, section_id, status);

CREATE TABLE IF NOT EXISTS section_writing_verifications (
    id                          BIGSERIAL PRIMARY KEY,
    tenant_id                   TEXT NOT NULL DEFAULT 'default',
    task_id                     BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    section_id                  BIGINT NOT NULL,
    section_rev_id              BIGINT NOT NULL,
    rev_no                      INT NOT NULL,
    storyboard_id               BIGINT,
    rule_version                TEXT NOT NULL DEFAULT 'd3-writing-v1',
    verdict                     TEXT NOT NULL DEFAULT 'failed'
                                CHECK (verdict IN ('passed','failed','stale')),
    stale_reason                TEXT NOT NULL DEFAULT '',
    ungrounded_claim_count      INT NOT NULL DEFAULT 0 CHECK (ungrounded_claim_count >= 0),
    fact_binding_rate           DOUBLE PRECISION NOT NULL DEFAULT 1.0,
    commitment_binding_rate     DOUBLE PRECISION NOT NULL DEFAULT 1.0,
    specificity_ratio           DOUBLE PRECISION NOT NULL DEFAULT 1.0,
    kb_similarity_max           DOUBLE PRECISION NOT NULL DEFAULT 0.0,
    boilerplate_similarity_max  DOUBLE PRECISION NOT NULL DEFAULT 0.0,
    claims                      JSONB NOT NULL DEFAULT '[]'::jsonb,
    metrics                     JSONB NOT NULL DEFAULT '{}'::jsonb,
    reasons                     JSONB NOT NULL DEFAULT '[]'::jsonb,
    created_by                  TEXT NOT NULL DEFAULT 'system',
    updated_by                  TEXT NOT NULL DEFAULT 'system',
    approved_by                 TEXT NOT NULL DEFAULT '',
    created_at                  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at                  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (tenant_id, section_rev_id),
    FOREIGN KEY (tenant_id, section_id) REFERENCES proposal_sections(tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id, section_rev_id) REFERENCES section_revisions(tenant_id, id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_section_writing_verifications_task_section
    ON section_writing_verifications(tenant_id, task_id, section_id, rev_no DESC);

-- 已通过粉队评审的故事板内容不可原地篡改；后续修订须将旧版置为 superseded 并新建版本，或由上游变更置为 stale。
CREATE OR REPLACE FUNCTION d3_storyboard_immutable() RETURNS trigger AS $$
BEGIN
    IF OLD.status = 'pink_approved' AND
       (NEW.theme_sentence, NEW.scoring_points, NEW.admissibility_item_ids, NEW.evidence_ids, NEW.commitment_ids,
        NEW.chart_plans, NEW.created_by, NEW.submitted_by, NEW.approved_by, NEW.version)
       IS DISTINCT FROM
       (OLD.theme_sentence, OLD.scoring_points, OLD.admissibility_item_ids, OLD.evidence_ids, OLD.commitment_ids,
        OLD.chart_plans, OLD.created_by, OLD.submitted_by, OLD.approved_by, OLD.version) THEN
        RAISE EXCEPTION 'pink_approved storyboard % is immutable; create a new version instead', OLD.id USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_d3_storyboard_immutable ON section_storyboards;
CREATE TRIGGER trg_d3_storyboard_immutable BEFORE UPDATE ON section_storyboards
    FOR EACH ROW EXECUTE FUNCTION d3_storyboard_immutable();

-- D3 级联失效：可采性运行失效时，引用该运行明細的故事板与写作核验同步置为 stale。
CREATE OR REPLACE FUNCTION d3_run_invalidated() RETURNS trigger AS $$
DECLARE
    why TEXT;
    item_ids BIGINT[];
    affected_sections BIGINT[];
BEGIN
    IF NEW.status NOT IN ('stale','superseded') OR OLD.status <> 'current' THEN
        RETURN NULL;
    END IF;
    why := format('admissibility run %s is %s', NEW.id, NEW.status);
    SELECT COALESCE(array_agg(id), '{}') INTO item_ids
      FROM admissibility_items
     WHERE tenant_id = NEW.tenant_id AND run_id = NEW.id;
    IF array_length(item_ids, 1) IS NULL THEN
        RETURN NULL;
    END IF;
    UPDATE section_storyboards SET status = 'stale', stale_reason = why, updated_by = 'system', updated_at = NOW()
        WHERE tenant_id = NEW.tenant_id AND status IN ('draft','pending_pink_review','pink_approved')
          AND admissibility_item_ids && item_ids;
    SELECT COALESCE(array_agg(DISTINCT section_id), '{}') INTO affected_sections
      FROM section_storyboards
     WHERE tenant_id = NEW.tenant_id AND admissibility_item_ids && item_ids;
    UPDATE section_writing_verifications SET verdict = 'stale', stale_reason = why, updated_by = 'system', updated_at = NOW()
        WHERE tenant_id = NEW.tenant_id AND verdict <> 'stale'
          AND (section_id = ANY(affected_sections) OR EXISTS (
              SELECT 1 FROM section_revision_evidence sre
               WHERE sre.tenant_id = section_writing_verifications.tenant_id
                 AND sre.section_rev_id = section_writing_verifications.section_rev_id
                 AND sre.admissibility_item_id = ANY(item_ids)
          ));
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_d3_run_invalidated ON admissibility_runs;
CREATE TRIGGER trg_d3_run_invalidated AFTER UPDATE OF status ON admissibility_runs
    FOR EACH ROW EXECUTE FUNCTION d3_run_invalidated();

-- D3 级联失效：承诺台账变化时，引用该台账的故事板与写作核验同步置为 stale。
CREATE OR REPLACE FUNCTION d3_commitment_changed() RETURNS trigger AS $$
DECLARE
    t TEXT; cid BIGINT; why TEXT;
BEGIN
    IF TG_OP = 'DELETE' THEN
        t := OLD.tenant_id; cid := OLD.id;
    ELSE
        t := NEW.tenant_id; cid := NEW.id;
        IF TG_OP = 'UPDATE' AND
           (NEW.committed_value, NEW.tender_threshold_value, NEW.threshold_cmp, NEW.status, NEW.capability_limit,
            NEW.capability_basis, NEW.source_admissibility_item_ids, NEW.version, NEW.unit, NEW.metric)
           IS NOT DISTINCT FROM
           (OLD.committed_value, OLD.tender_threshold_value, OLD.threshold_cmp, OLD.status, OLD.capability_limit,
            OLD.capability_basis, OLD.source_admissibility_item_ids, OLD.version, OLD.unit, OLD.metric) THEN
            RETURN NULL;
        END IF;
    END IF;
    why := format('commitment %s changed', cid);
    UPDATE section_storyboards SET status = 'stale', stale_reason = why, updated_by = 'system', updated_at = NOW()
        WHERE tenant_id = t AND status IN ('draft','pending_pink_review','pink_approved')
          AND commitment_ids @> ARRAY[cid];
    UPDATE section_writing_verifications wv SET verdict = 'stale', stale_reason = why, updated_by = 'system', updated_at = NOW()
        WHERE wv.tenant_id = t AND wv.verdict <> 'stale'
          AND (
              EXISTS (
                  SELECT 1 FROM section_revisions rv
                   WHERE rv.tenant_id = wv.tenant_id AND rv.id = wv.section_rev_id
                     AND rv.commitment_ids @> ARRAY[cid]
              )
              OR EXISTS (
                  SELECT 1 FROM section_storyboards sb
                   WHERE sb.tenant_id = wv.tenant_id AND sb.id = wv.storyboard_id
                     AND sb.commitment_ids @> ARRAY[cid]
              )
          );
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_d3_commitment_changed ON commitment_ledger;
CREATE TRIGGER trg_d3_commitment_changed AFTER UPDATE OR DELETE ON commitment_ledger
    FOR EACH ROW EXECUTE FUNCTION d3_commitment_changed();

-- D3 级联失效：知识库证据失效或矛盾时，引用该证据的故事板与写作核验置为 stale。
CREATE OR REPLACE FUNCTION d3_retrieval_evidence_changed() RETURNS trigger AS $$
DECLARE why TEXT;
BEGIN
    IF NEW.status IN ('stale','contested') OR NEW.relation = 'contradicts' THEN
        why := format('retrieval evidence %s is %s/%s', NEW.id, NEW.status, NEW.relation);
        UPDATE section_storyboards SET status = 'stale', stale_reason = why, updated_by = 'system', updated_at = NOW()
            WHERE tenant_id = NEW.tenant_id AND status IN ('draft','pending_pink_review','pink_approved')
              AND evidence_ids @> ARRAY[NEW.id];
        UPDATE section_writing_verifications wv SET verdict = 'stale', stale_reason = why, updated_by = 'system', updated_at = NOW()
            WHERE wv.tenant_id = NEW.tenant_id AND wv.verdict <> 'stale'
              AND (
                  EXISTS (
                      SELECT 1 FROM section_revisions rv
                       WHERE rv.tenant_id = wv.tenant_id AND rv.id = wv.section_rev_id
                         AND rv.evidence_ids @> ARRAY[NEW.id]
                  )
                  OR EXISTS (
                      SELECT 1 FROM section_storyboards sb
                       WHERE sb.tenant_id = wv.tenant_id AND sb.id = wv.storyboard_id
                         AND sb.evidence_ids @> ARRAY[NEW.id]
                  )
              );
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_d3_retrieval_evidence_changed ON retrieval_evidence;
CREATE TRIGGER trg_d3_retrieval_evidence_changed AFTER UPDATE OF status, relation ON retrieval_evidence
    FOR EACH ROW EXECUTE FUNCTION d3_retrieval_evidence_changed();

-- D3 级联失效：章节因提纲变更置为 stale_outline 时，故事板与写作核验同步置为 stale。
CREATE OR REPLACE FUNCTION d3_section_outline_stale() RETURNS trigger AS $$
BEGIN
    IF NEW.status = 'stale_outline' AND OLD.status <> 'stale_outline' THEN
        UPDATE section_storyboards SET status = 'stale', stale_reason = 'outline superseded', updated_by = 'system', updated_at = NOW()
            WHERE tenant_id = NEW.tenant_id AND section_id = NEW.id
              AND status IN ('draft','pending_pink_review','pink_approved');
        UPDATE section_writing_verifications SET verdict = 'stale', stale_reason = 'outline superseded', updated_by = 'system', updated_at = NOW()
            WHERE tenant_id = NEW.tenant_id AND section_id = NEW.id AND verdict <> 'stale';
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_d3_section_outline_stale ON proposal_sections;
CREATE TRIGGER trg_d3_section_outline_stale AFTER UPDATE OF status ON proposal_sections
    FOR EACH ROW EXECUTE FUNCTION d3_section_outline_stale();

-- D3 级联失效：要求项变更时，关联章节的故事板与写作核验同步置为 stale。
CREATE OR REPLACE FUNCTION d3_requirement_changed() RETURNS trigger AS $$
DECLARE why TEXT;
BEGIN
    IF (NEW.code, NEW.category, NEW.title, NEW.description, NEW.is_mandatory, NEW.score_weight, NEW.score_group,
        NEW.source_hash, NEW.proof_rules, NEW.scoring_tiers, NEW.eval_kind, NEW.parent_id, NEW.superseded_by_id, NEW.review_status)
       IS NOT DISTINCT FROM
       (OLD.code, OLD.category, OLD.title, OLD.description, OLD.is_mandatory, OLD.score_weight, OLD.score_group,
        OLD.source_hash, OLD.proof_rules, OLD.scoring_tiers, OLD.eval_kind, OLD.parent_id, OLD.superseded_by_id, OLD.review_status) THEN
        RETURN NULL;
    END IF;
    why := format('requirement %s changed', NEW.code);
    UPDATE section_storyboards sb SET status = 'stale', stale_reason = why, updated_by = 'system', updated_at = NOW()
        WHERE sb.tenant_id = NEW.tenant_id AND sb.task_id = NEW.task_id
          AND sb.status IN ('draft','pending_pink_review','pink_approved')
          AND EXISTS (
              SELECT 1 FROM proposal_section_requirements m
               WHERE m.tenant_id = sb.tenant_id AND m.section_id = sb.section_id
                 AND m.requirement_id = NEW.id
          );
    UPDATE section_writing_verifications wv SET verdict = 'stale', stale_reason = why, updated_by = 'system', updated_at = NOW()
        WHERE wv.tenant_id = NEW.tenant_id AND wv.task_id = NEW.task_id AND wv.verdict <> 'stale'
          AND EXISTS (
              SELECT 1 FROM proposal_section_requirements m
               WHERE m.tenant_id = wv.tenant_id AND m.section_id = wv.section_id
                 AND m.requirement_id = NEW.id
          );
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_d3_requirement_changed ON requirement_items;
CREATE TRIGGER trg_d3_requirement_changed AFTER UPDATE ON requirement_items
    FOR EACH ROW EXECUTE FUNCTION d3_requirement_changed();

-- R1: durable section writing requests; domain C, independent from tool approvals.
CREATE TABLE IF NOT EXISTS section_write_jobs (
    id BIGSERIAL PRIMARY KEY,
    tenant_id TEXT NOT NULL DEFAULT 'default',
    task_id BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    section_id BIGINT NOT NULL,
    section_code TEXT NOT NULL,
    base_rev_no INT NOT NULL,
    outline_version INT NOT NULL,
    request_key TEXT NOT NULL,
    instruction TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'queued' CHECK(status IN ('queued','running','awaiting_review','candidate','blocked','cancelled')),
    attempts INT NOT NULL DEFAULT 0,
    max_attempts INT NOT NULL DEFAULT 3 CHECK(max_attempts BETWEEN 1 AND 10),
    deadline_at TIMESTAMPTZ NOT NULL DEFAULT now()+interval '30 minutes',
    lease_token BIGINT NOT NULL DEFAULT 0,
    lease_until TIMESTAMPTZ,
    result_rev_no INT,
    error TEXT NOT NULL DEFAULT '',
    created_by TEXT NOT NULL DEFAULT '',
    updated_by TEXT NOT NULL DEFAULT '',
    approved_by TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(tenant_id,task_id,request_key),
    FOREIGN KEY(tenant_id,task_id,section_id) REFERENCES proposal_sections(tenant_id,task_id,id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX IF NOT EXISTS section_write_jobs_active ON section_write_jobs(tenant_id,section_id) WHERE status IN ('queued','running');
CREATE INDEX IF NOT EXISTS section_write_jobs_pending ON section_write_jobs(task_id,id) WHERE status IN ('queued','running');

-- R1: durable per-recipient operator inputs, domain B.
CREATE TABLE IF NOT EXISTS intent_inputs (
 id BIGSERIAL PRIMARY KEY,
 tenant_id TEXT NOT NULL DEFAULT 'default',
 exploration_id BIGINT NOT NULL REFERENCES explorations(id) ON DELETE CASCADE,
 hint_id BIGINT NOT NULL REFERENCES exploration_nodes(id) ON DELETE CASCADE,
 intent_id BIGINT NOT NULL REFERENCES exploration_nodes(id) ON DELETE CASCADE,
 body TEXT NOT NULL,
 state TEXT NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','delivered','acknowledged','returned')),
 delivery_epoch BIGINT NOT NULL DEFAULT 0,
 created_by TEXT NOT NULL DEFAULT '', updated_by TEXT NOT NULL DEFAULT '', approved_by TEXT NOT NULL DEFAULT '',
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(), updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 UNIQUE(tenant_id,hint_id,intent_id)
);
CREATE INDEX IF NOT EXISTS intent_inputs_pending ON intent_inputs(exploration_id,intent_id,id) WHERE state IN ('pending','delivered');

-- Producer provenance, domain B; referenced text remains in domain C.
CREATE TABLE IF NOT EXISTS intent_section_outputs (
 tenant_id TEXT NOT NULL DEFAULT 'default',
 intent_id BIGINT NOT NULL REFERENCES exploration_nodes(id) ON DELETE CASCADE,
 lease_epoch BIGINT NOT NULL,
 section_rev_id BIGINT NOT NULL REFERENCES section_revisions(id) ON DELETE CASCADE,
 created_by TEXT NOT NULL DEFAULT '', updated_by TEXT NOT NULL DEFAULT '', approved_by TEXT NOT NULL DEFAULT '',
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 PRIMARY KEY(tenant_id,section_rev_id)
);
CREATE INDEX IF NOT EXISTS intent_section_outputs_producer ON intent_section_outputs(tenant_id,intent_id);

-- A stopped recipient must not strand an active Hint after a crash or direct
-- state transition. The Planner can consume/replan the returned instruction.
-- S2: blocked + waiting_job (an Intent parked on background jobs) keeps its
-- inputs; leaving that wait for any other non-active state returns them, even
-- when only blocked_reason changes.
CREATE OR REPLACE FUNCTION return_terminal_intent_inputs() RETURNS trigger AS $$
BEGIN
 IF NEW.kind='intent' AND NEW.state NOT IN ('running','open','paused')
    AND NOT (NEW.state='blocked' AND NEW.blocked_reason IS NOT DISTINCT FROM 'waiting_job')
    AND (OLD.state, OLD.blocked_reason) IS DISTINCT FROM (NEW.state, NEW.blocked_reason) THEN
  UPDATE intent_inputs SET state='returned',updated_at=now()
   WHERE intent_id=NEW.id AND state IN ('pending','delivered');
 END IF;
 RETURN NULL;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_return_terminal_intent_inputs ON exploration_nodes;
CREATE TRIGGER trg_return_terminal_intent_inputs AFTER UPDATE OF state, blocked_reason ON exploration_nodes
 FOR EACH ROW EXECUTE FUNCTION return_terminal_intent_inputs();

-- S2 (AGENT_TOOL_SKILLS_PLAN 5.1 / 5.3): durable Agent job subscriptions and
-- terminal events, domain B. Polymorphic job references cannot use a plain FK;
-- the Go layer validates task, tenant and the live intent lease inside the
-- submitting transaction. A kind is listed only once its validation exists;
-- S3 adds chart_render (chart_render_jobs) and replaces the CHECK on old databases.
CREATE TABLE IF NOT EXISTS agent_job_subscriptions (
    id                   BIGSERIAL PRIMARY KEY,
    tenant_id            TEXT NOT NULL DEFAULT 'default',
    task_id              BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    exploration_id       BIGINT NOT NULL REFERENCES explorations(id) ON DELETE CASCADE,
    intent_id            BIGINT NOT NULL REFERENCES exploration_nodes(id) ON DELETE CASCADE,
    created_intent_epoch BIGINT NOT NULL,
    job_kind             TEXT NOT NULL,
    job_id               BIGINT NOT NULL,
    target_ref           JSONB NOT NULL DEFAULT '{}',
    expected_version     BIGINT,
    required             BOOLEAN NOT NULL DEFAULT true,
    state                TEXT NOT NULL DEFAULT 'waiting',
    created_by           TEXT NOT NULL DEFAULT '',
    updated_by           TEXT NOT NULL DEFAULT '',
    approved_by          TEXT NOT NULL DEFAULT '',
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT ck_agent_job_subscriptions_kind CHECK (job_kind IN ('tender_parse','proposal_export','chart_render')),
    CONSTRAINT ck_agent_job_subscriptions_state CHECK (state IN ('waiting','notified','acknowledged','cancelled')),
    CONSTRAINT uq_agent_job_subscriptions_intent_job UNIQUE (tenant_id, intent_id, job_kind, job_id),
    CONSTRAINT uq_agent_job_subscriptions_tenant_id UNIQUE (tenant_id, id)
);
CREATE INDEX IF NOT EXISTS idx_agent_job_subscriptions_job ON agent_job_subscriptions(tenant_id, job_kind, job_id) WHERE state = 'waiting';
CREATE INDEX IF NOT EXISTS idx_agent_job_subscriptions_intent ON agent_job_subscriptions(intent_id, state, id);
CREATE INDEX IF NOT EXISTS idx_agent_job_subscriptions_task ON agent_job_subscriptions(tenant_id, task_id, id DESC);
DROP TRIGGER IF EXISTS trg_agent_job_subscriptions_upd ON agent_job_subscriptions;
CREATE TRIGGER trg_agent_job_subscriptions_upd BEFORE UPDATE ON agent_job_subscriptions
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- One terminal event per subscription; ownership is read through the
-- subscription. The terminal payload never changes after insertion.
CREATE TABLE IF NOT EXISTS agent_job_events (
    id              BIGSERIAL PRIMARY KEY,
    tenant_id       TEXT NOT NULL DEFAULT 'default',
    subscription_id BIGINT NOT NULL,
    terminal_status TEXT NOT NULL CHECK (terminal_status IN ('succeeded','failed','cancelled','superseded')),
    business_state  TEXT NOT NULL DEFAULT '',
    payload         JSONB NOT NULL DEFAULT '{}',
    state           TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','delivered','acknowledged','expired')),
    delivery_epoch  BIGINT NOT NULL DEFAULT 0,
    delivered_at    TIMESTAMPTZ,
    acknowledged_at TIMESTAMPTZ,
    created_by      TEXT NOT NULL DEFAULT '',
    updated_by      TEXT NOT NULL DEFAULT '',
    approved_by     TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_agent_job_events_subscription UNIQUE (subscription_id),
    CONSTRAINT fk_agent_job_events_subscription FOREIGN KEY (tenant_id, subscription_id)
        REFERENCES agent_job_subscriptions(tenant_id, id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_agent_job_events_open ON agent_job_events(subscription_id) WHERE state IN ('pending','delivered');
DROP TRIGGER IF EXISTS trg_agent_job_events_upd ON agent_job_events;
CREATE TRIGGER trg_agent_job_events_upd BEFORE UPDATE ON agent_job_events
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE OR REPLACE FUNCTION agent_job_event_immutable() RETURNS trigger AS $$
BEGIN
    IF (NEW.tenant_id, NEW.subscription_id, NEW.terminal_status, NEW.business_state, NEW.payload, NEW.created_by, NEW.created_at)
       IS DISTINCT FROM
       (OLD.tenant_id, OLD.subscription_id, OLD.terminal_status, OLD.business_state, OLD.payload, OLD.created_by, OLD.created_at) THEN
        RAISE EXCEPTION 'agent job event % is immutable', OLD.id USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_agent_job_events_immutable ON agent_job_events;
CREATE TRIGGER trg_agent_job_events_immutable BEFORE UPDATE ON agent_job_events
    FOR EACH ROW EXECUTE FUNCTION agent_job_event_immutable();

-- Terminal outcome of one job, shared by subscribe-after-terminal (Go) and the
-- terminal triggers below; terminal_status is NULL while the job is active.
-- Export results stay preview_only until S4 adds the strict final-draft policy.
CREATE OR REPLACE FUNCTION agent_job_outcome(k TEXT, jid BIGINT, OUT terminal_status TEXT, OUT business_state TEXT, OUT payload JSONB) AS $$
BEGIN
    IF k = 'tender_parse' THEN
        SELECT j.status,
               CASE j.status
                   WHEN 'succeeded' THEN CASE r.status WHEN 'ready' THEN 'candidate_pending_review' WHEN 'adopted' THEN 'adopted'
                                                       WHEN 'failed' THEN 'failed' ELSE 'completed' END
                   ELSE j.status END,
               jsonb_build_object('status', j.status, 'attempts', j.attempts, 'max_attempts', j.max_attempts,
                   'record_id', j.record_id, 'record_status', r.status, 'error', left(j.error, 500))
          INTO terminal_status, business_state, payload
          FROM fixed_file_jobs j LEFT JOIN file_processing_records r ON r.id = j.record_id
         WHERE j.id = jid AND j.status IN ('succeeded','failed','cancelled');
    ELSIF k = 'proposal_export' THEN
        SELECT j.status,
               CASE j.status WHEN 'succeeded' THEN 'preview_only' ELSE j.status END,
               jsonb_build_object('status', j.status, 'attempts', j.attempts, 'max_attempts', j.max_attempts, 'format', j.format,
                   'outline_version', j.outline_version, 'toc_refreshed', j.toc_refreshed, 'artifact_hash', j.artifact_hash,
                   'artifact_size', j.artifact_size, 'error', left(j.error, 500))
          INTO terminal_status, business_state, payload
          FROM export_jobs j
         WHERE j.id = jid AND j.status IN ('succeeded','failed','cancelled');
    ELSIF k = 'chart_render' THEN
        -- S3: a succeeded render is only chart_ready while the chart still matches its job source and the
        -- section's current revision; a later revision turns the same result into chart_stale.
        SELECT j.status,
               CASE j.status
                   WHEN 'succeeded' THEN CASE WHEN c.source_hash = j.source_hash AND c.rev_no = s.current_rev_no
                                                   AND c.render_status = 'rendered' THEN 'chart_ready' ELSE 'chart_stale' END
                   ELSE j.status END,
               jsonb_build_object('status', j.status, 'attempts', j.attempts, 'max_attempts', j.max_attempts, 'chart_id', j.chart_id,
                   'section_id', c.section_id, 'section_code', s.section_code, 'rev_no', c.rev_no, 'current_rev_no', s.current_rev_no,
                   'render_status', c.render_status, 'render_format', c.render_format, 'width_px', c.width_px, 'height_px', c.height_px,
                   'artifact_hash', c.artifact_hash, 'source_hash', j.source_hash, 'error_code', j.error_code, 'error', left(j.error, 500))
          INTO terminal_status, business_state, payload
          FROM chart_render_jobs j
          JOIN proposal_charts c ON c.tenant_id = j.tenant_id AND c.task_id = j.task_id AND c.id = j.chart_id
          JOIN proposal_sections s ON s.tenant_id = c.tenant_id AND s.id = c.section_id
         WHERE j.id = jid AND j.status IN ('succeeded','failed','cancelled','superseded');
    END IF;
END;
$$ LANGUAGE plpgsql;

-- Every terminal transition of a subscribed job publishes inside the job's own
-- transaction: success, permanent failure, cancellation, deadline and attempt
-- exhaustion alike (S3 adds superseded). Returning to queued is not terminal.
-- Only subscriptions and events are written; exploration_nodes are never locked here.
CREATE OR REPLACE FUNCTION agent_job_terminal() RETURNS trigger AS $$
DECLARE
    o RECORD;
BEGIN
    IF NEW.status IN ('succeeded','failed','cancelled','superseded') AND OLD.status NOT IN ('succeeded','failed','cancelled','superseded') THEN
        SELECT * INTO o FROM agent_job_outcome(TG_ARGV[0], NEW.id);
        IF o.terminal_status IS NOT NULL THEN
            WITH s AS (
                UPDATE agent_job_subscriptions SET state = 'notified', updated_by = 'job-terminal'
                 WHERE tenant_id = NEW.tenant_id AND job_kind = TG_ARGV[0] AND job_id = NEW.id AND state = 'waiting'
                RETURNING tenant_id, id
            )
            INSERT INTO agent_job_events(tenant_id, subscription_id, terminal_status, business_state, payload, created_by, updated_by)
            SELECT s.tenant_id, s.id, o.terminal_status, o.business_state, o.payload, 'job-terminal', 'job-terminal' FROM s
            ON CONFLICT (subscription_id) DO NOTHING;
        END IF;
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_fixed_file_jobs_agent_terminal ON fixed_file_jobs;
CREATE TRIGGER trg_fixed_file_jobs_agent_terminal AFTER UPDATE OF status ON fixed_file_jobs
    FOR EACH ROW EXECUTE FUNCTION agent_job_terminal('tender_parse');
DROP TRIGGER IF EXISTS trg_export_jobs_agent_terminal ON export_jobs;
CREATE TRIGGER trg_export_jobs_agent_terminal AFTER UPDATE OF status ON export_jobs
    FOR EACH ROW EXECUTE FUNCTION agent_job_terminal('proposal_export');

-- ---------------------------------------------------------------------------
-- S3 (AGENT_TOOL_SKILLS_PLAN 3.2 / 4.3 / 5.1~5.2): durable chart render jobs, domain C.
--   chart_render_jobs  one render request for one chart. The revision is read from the bound chart (composite FK),
--                      never copied. request_key is unique for the job's whole life (terminal replays return the
--                      original job); at most one queued/running job per chart. deadline_at and max_attempts are
--                      fixed at first submission; lease_token fences every write-back. Rendering happens outside
--                      database transactions (export.ChartRunner).
--   proposal_charts.plan_code       code of an HTTP section writer chart plan (unique per revision).
--   section_write_jobs.chart_plans  the plans returned with the draft, kept with candidate results too.
-- ---------------------------------------------------------------------------
ALTER TABLE proposal_charts ADD COLUMN IF NOT EXISTS plan_code TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX IF NOT EXISTS uq_proposal_charts_plan_code ON proposal_charts(tenant_id, section_rev_id, plan_code) WHERE plan_code <> '';
ALTER TABLE section_write_jobs ADD COLUMN IF NOT EXISTS chart_plans JSONB NOT NULL DEFAULT '[]'::jsonb;

CREATE TABLE IF NOT EXISTS chart_render_jobs (
    id           BIGSERIAL PRIMARY KEY,
    tenant_id    TEXT NOT NULL DEFAULT 'default',
    task_id      BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    chart_id     BIGINT NOT NULL,
    request_key  TEXT NOT NULL CHECK (request_key <> '' AND length(request_key) <= 200),
    request_hash TEXT NOT NULL CHECK (request_hash <> ''),
    source_hash  TEXT NOT NULL CHECK (source_hash <> ''),
    executor     TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL DEFAULT 'queued'
                 CHECK (status IN ('queued','running','succeeded','failed','cancelled','superseded')),
    attempts     INT NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    max_attempts INT NOT NULL DEFAULT 3 CHECK (max_attempts BETWEEN 1 AND 10),
    deadline_at  TIMESTAMPTZ NOT NULL,
    lease_owner  TEXT NOT NULL DEFAULT '',
    lease_token  BIGINT NOT NULL DEFAULT 0,
    lease_until  TIMESTAMPTZ,
    error_code   TEXT NOT NULL DEFAULT '',
    error        TEXT NOT NULL DEFAULT '',
    started_at   TIMESTAMPTZ,
    finished_at  TIMESTAMPTZ,
    created_by   TEXT NOT NULL DEFAULT '',
    updated_by   TEXT NOT NULL DEFAULT '',
    approved_by  TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_chart_render_jobs_request UNIQUE (tenant_id, task_id, request_key),
    CONSTRAINT uq_chart_render_jobs_task_id UNIQUE (tenant_id, task_id, id),
    CONSTRAINT ck_chart_render_jobs_lease CHECK (status <> 'running' OR lease_until IS NOT NULL),
    CONSTRAINT fk_chart_render_jobs_chart FOREIGN KEY (tenant_id, task_id, chart_id)
        REFERENCES proposal_charts(tenant_id, task_id, id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_chart_render_jobs_active ON chart_render_jobs(tenant_id, chart_id) WHERE status IN ('queued','running');
CREATE INDEX IF NOT EXISTS idx_chart_render_jobs_queue ON chart_render_jobs(id) WHERE status IN ('queued','running');
CREATE INDEX IF NOT EXISTS idx_chart_render_jobs_task ON chart_render_jobs(tenant_id, task_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_chart_render_jobs_chart ON chart_render_jobs(tenant_id, chart_id, id DESC);
DROP TRIGGER IF EXISTS trg_chart_render_jobs_upd ON chart_render_jobs;
CREATE TRIGGER trg_chart_render_jobs_upd BEFORE UPDATE ON chart_render_jobs
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- The frozen request (input, executor, deadline and attempt budget) never changes, and a terminal job never
-- changes status again: a late success, failure or heartbeat cannot rewrite a cancelled or superseded job.
CREATE OR REPLACE FUNCTION chart_render_jobs_immutable() RETURNS trigger AS $$
BEGIN
    IF (NEW.tenant_id, NEW.task_id, NEW.chart_id, NEW.request_key, NEW.request_hash, NEW.source_hash, NEW.executor,
        NEW.max_attempts, NEW.deadline_at, NEW.created_by, NEW.created_at)
       IS DISTINCT FROM
       (OLD.tenant_id, OLD.task_id, OLD.chart_id, OLD.request_key, OLD.request_hash, OLD.source_hash, OLD.executor,
        OLD.max_attempts, OLD.deadline_at, OLD.created_by, OLD.created_at) THEN
        RAISE EXCEPTION 'chart render job % input is immutable', OLD.id USING ERRCODE = 'check_violation';
    END IF;
    IF OLD.status IN ('succeeded','failed','cancelled','superseded') AND NEW.status IS DISTINCT FROM OLD.status THEN
        RAISE EXCEPTION 'chart render job % is already %', OLD.id, OLD.status USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_chart_render_jobs_immutable ON chart_render_jobs;
CREATE TRIGGER trg_chart_render_jobs_immutable BEFORE UPDATE ON chart_render_jobs
    FOR EACH ROW EXECUTE FUNCTION chart_render_jobs_immutable();

DROP TRIGGER IF EXISTS trg_chart_render_jobs_agent_terminal ON chart_render_jobs;
CREATE TRIGGER trg_chart_render_jobs_agent_terminal AFTER UPDATE OF status ON chart_render_jobs
    FOR EACH ROW EXECUTE FUNCTION agent_job_terminal('chart_render');

-- Old databases created the subscription kind CHECK without chart_render.
ALTER TABLE agent_job_subscriptions DROP CONSTRAINT IF EXISTS ck_agent_job_subscriptions_kind;
ALTER TABLE agent_job_subscriptions ADD CONSTRAINT ck_agent_job_subscriptions_kind
    CHECK (job_kind IN ('tender_parse','proposal_export','chart_render'));
