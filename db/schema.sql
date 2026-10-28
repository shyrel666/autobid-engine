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
CREATE UNIQUE INDEX IF NOT EXISTS uq_company_credentials_code ON company_credentials(category, code) WHERE code <> '';
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
ALTER TABLE tender_documents
    ADD COLUMN IF NOT EXISTS tenant_id  TEXT NOT NULL DEFAULT 'default',
    ADD COLUMN IF NOT EXISTS file_name  TEXT,
    ADD COLUMN IF NOT EXISTS mime_type  TEXT,
    ADD COLUMN IF NOT EXISTS page_count INT,
    ADD COLUMN IF NOT EXISTS parse_meta JSONB,
    ADD COLUMN IF NOT EXISTS created_by TEXT;
DROP INDEX IF EXISTS uq_tender_documents_content;
CREATE UNIQUE INDEX IF NOT EXISTS uq_tender_documents_tenant_content
    ON tender_documents(tenant_id, task_id, tender_no, lot_no, content_hash);

ALTER TABLE document_chunks
    ADD COLUMN IF NOT EXISTS page_start  INT,
    ADD COLUMN IF NOT EXISTS page_end    INT,
    ADD COLUMN IF NOT EXISTS tables_json JSONB,
    ADD COLUMN IF NOT EXISTS bbox_meta   JSONB;
UPDATE document_chunks SET page_start = page_no, page_end = page_no WHERE page_start IS NULL;

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
DROP TRIGGER IF EXISTS trg_tender_retrieval_stale ON tender_documents;
CREATE TRIGGER trg_tender_retrieval_stale AFTER UPDATE OR DELETE ON tender_documents FOR EACH ROW EXECUTE FUNCTION retrieval_source_changed();
DROP TRIGGER IF EXISTS trg_chunks_retrieval_stale ON document_chunks;
CREATE TRIGGER trg_chunks_retrieval_stale AFTER UPDATE OR DELETE ON document_chunks FOR EACH ROW EXECUTE FUNCTION retrieval_source_changed();
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

CREATE OR REPLACE FUNCTION requirement_items_changed() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        PERFORM requirement_manifest_bump(OLD.tenant_id, OLD.task_id, OLD.updated_by);
        RETURN NULL;
    END IF;
    IF TG_OP = 'UPDATE' AND
       (NEW.code, NEW.category, NEW.title, NEW.description, NEW.is_mandatory, NEW.score_weight,
        NEW.source_doc_id, NEW.source_chunk_id, NEW.source_hash, NEW.source_loc)
       IS NOT DISTINCT FROM
       (OLD.code, OLD.category, OLD.title, OLD.description, OLD.is_mandatory, OLD.score_weight,
        OLD.source_doc_id, OLD.source_chunk_id, OLD.source_hash, OLD.source_loc) THEN
        RETURN NULL; -- 证据绑定与复核状态变化不改变清单内容
    END IF;
    PERFORM requirement_manifest_bump(NEW.tenant_id, NEW.task_id, NEW.updated_by);
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_requirement_items_changed ON requirement_items;
CREATE TRIGGER trg_requirement_items_changed AFTER INSERT OR UPDATE OR DELETE ON requirement_items
    FOR EACH ROW EXECUTE FUNCTION requirement_items_changed();

-- 要求的招标原文分块被修改或删除：要求标记待复核并使清单 / 大纲失效（BEFORE DELETE 先于 SET NULL 外键动作）。
CREATE OR REPLACE FUNCTION requirement_source_changed() RETURNS trigger AS $$
DECLARE r RECORD;
BEGIN
    IF TG_OP = 'UPDATE' AND (NEW.content, NEW.section_code, NEW.section_title, NEW.page_start, NEW.page_end, NEW.tables_json)
        IS NOT DISTINCT FROM (OLD.content, OLD.section_code, OLD.section_title, OLD.page_start, OLD.page_end, OLD.tables_json) THEN
        RETURN NULL;
    END IF;
    IF EXISTS (SELECT 1 FROM tasks WHERE id = OLD.task_id) THEN
        FOR r IN UPDATE requirement_items SET review_status = 'needs_revision'
                     WHERE source_chunk_id = OLD.id RETURNING tenant_id, task_id LOOP
            PERFORM requirement_manifest_bump(r.tenant_id, r.task_id, 'system');
        END LOOP;
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

-- 失效路径 1：要求实质变更 -> 该要求的矩阵行与漏项结论 stale（触发器名排在 trg_requirement_items_changed 之前，先记录要求原因）。
CREATE OR REPLACE FUNCTION compliance_requirement_changed() RETURNS trigger AS $$
BEGIN
    IF (NEW.code, NEW.category, NEW.title, NEW.description, NEW.is_mandatory, NEW.score_weight,
        NEW.source_doc_id, NEW.source_chunk_id, NEW.source_hash, NEW.source_loc)
       IS NOT DISTINCT FROM
       (OLD.code, OLD.category, OLD.title, OLD.description, OLD.is_mandatory, OLD.score_weight,
        OLD.source_doc_id, OLD.source_chunk_id, OLD.source_hash, OLD.source_loc) THEN
        RETURN NULL;
    END IF;
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
-- Phase 6B：标书图表（标书正文域）。
--   proposal_charts   绑定到具体章节修订（section_rev_id + rev_no，复合外键）。章节产生新的有效修订后，绑定旧修订的图表
--                     在查询时判定为过期（rev_no <> 章节 current_rev_no），导出快照只收录与锁定修订一致的图表。
-- 不写入 assets 或 explorations/findings。
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
