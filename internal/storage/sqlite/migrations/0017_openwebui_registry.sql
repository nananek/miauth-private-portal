-- Issue #52 (OWUI-P): the Open WebUI registry — one instance/account
-- pair (a "workspace", ADR-0005 D9) and the models it offers, each
-- projected to Aria through an actors row of type 'openwebui_model'
-- (migration 0016).
--
-- No row is created by this migration and nothing is wired to it yet.
-- With OPENWEBUI_ENABLED off, which is the default, both tables stay
-- empty and no existing behaviour changes.
--
-- The two tables reference each other: a workspace names its default
-- model, and a model names its workspace. openwebui_workspaces is
-- created first and its foreign key is DEFERRABLE INITIALLY DEFERRED, so
-- one transaction can insert the workspace (with a NULL default), insert
-- the model, and point the default at it, without an intermediate state
-- that no ordering could avoid.
CREATE TABLE openwebui_workspaces (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    -- An HTTPS origin only, validated against the configured allowlist
    -- before it ever reaches here (ADR-0005 D11). UNIQUE is what lets
    -- startup reconciliation recognise an already-registered instance.
    base_url TEXT NOT NULL UNIQUE,
    -- The *name* of the configuration key holding the provider API key,
    -- never a credential (ADR-0005 D10).
    secret_ref TEXT NOT NULL,
    -- A fixed deployment-provisioned value, never inferred from
    -- base_url: it is the host half of a VirtualActor's handle, and it
    -- is presentation only.
    presentation_host TEXT NOT NULL,
    default_model_id TEXT,
    enabled INTEGER NOT NULL DEFAULT 0 CHECK (enabled IN (0, 1)),
    -- Outbound generation is Issue #53's; the column exists so its
    -- gate has somewhere to live, and stays 0 until then.
    generation_enabled INTEGER NOT NULL DEFAULT 0 CHECK (generation_enabled IN (0, 1)),
    -- Whether this instance has actually been observed to support each
    -- of ADR-0005 D3's two provider operations. Nothing is assumed from
    -- a version number.
    chat_create_status TEXT NOT NULL DEFAULT 'unverified'
        CHECK (chat_create_status IN ('unverified', 'verified', 'unsupported')),
    chat_continue_status TEXT NOT NULL DEFAULT 'unverified'
        CHECK (chat_continue_status IN ('unverified', 'verified', 'unsupported')),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    -- The composite target makes "a workspace's default model must
    -- belong to that workspace" a constraint rather than a Go check: the
    -- (id, default_model_id) pair has to match a real
    -- (workspace_id, id) pair in openwebui_models.
    FOREIGN KEY (id, default_model_id) REFERENCES openwebui_models (workspace_id, id)
        DEFERRABLE INITIALLY DEFERRED
);

CREATE TABLE openwebui_models (
    id TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES openwebui_workspaces (id),
    -- The provider's own model id, opaque and immutable: never
    -- regenerated from display_name, never parsed, never ordered by.
    external_model_id TEXT NOT NULL,
    display_name TEXT NOT NULL,
    -- The slug half of the VirtualActor handle. Both it and
    -- display_name may change without disturbing id or actor_id, which
    -- is the roadmap's stable-actor-ID requirement.
    actor_slug TEXT NOT NULL,
    actor_id TEXT NOT NULL UNIQUE REFERENCES actors (id),
    active INTEGER NOT NULL DEFAULT 1 CHECK (active IN (0, 1)),
    -- A JSON document. Unknown members are ignored on read and NULL-ish
    -- values decode as "nothing verified", so a row written by a newer
    -- build still loads (see domain.ParseOpenWebUIModelCapabilities).
    capabilities TEXT NOT NULL DEFAULT '{}',
    external_updated_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE (workspace_id, external_model_id),
    -- The target of openwebui_workspaces' composite foreign key above.
    UNIQUE (workspace_id, id),
    UNIQUE (workspace_id, actor_slug)
);
