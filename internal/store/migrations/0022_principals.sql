-- Principals: the identity bookkeeping behind human authentication (#56, plan
-- 31 slice 2). A row records that a verified human has been seen, and nothing
-- more.

-- Rows are JIT-provisioned: the first verified request upserts by
-- (issuer, subject) and refreshes email/display_name/last_seen_at. There is no
-- admin pre-registration step, because there is nothing to approve —
-- authorization comes from the token's claims on every request, so the row is a
-- record that someone appeared, never a grant that lets them in. Deleting one
-- revokes nothing and creating one grants nothing.

-- Roles are deliberately NOT a column. Storing them would make this table a
-- second, stale authority beside the IdP: a human demoted at the provider would
-- keep the role until something wrote the row again. The provider stays
-- authoritative per request.

-- (issuer, subject) is the identity, not the email: an address can be reassigned
-- to another human, while an OIDC subject is the provider's promise of a stable,
-- unique identifier within its issuer. Two providers may both mint subject "1",
-- which is why the pair is the key.

-- What is stored is what audit needs and nothing else — no token, no claim set,
-- no roles. Revoking the human at the IdP ends platform access when their token
-- expires; nothing here keeps them in. An operator may DELETE a stale row at any
-- time: sessions.created_by is plain text, not a foreign key, so audit history
-- survives the deletion as an opaque id. No retention timer ships with the
-- platform on purpose — an erasure regime wants the row gone quickly and an
-- audit regime wants it stable while created_by still resolves, and either
-- default is silently wrong for the other. docs/self-hosted-security.md
-- documents the last_seen_at-based DELETE an operator runs on their own
-- schedule.
CREATE TABLE principals (
    id            text PRIMARY KEY,          -- principal_… (domain.NewID)
    issuer        text NOT NULL,
    subject       text NOT NULL,
    email         text NOT NULL DEFAULT '',
    display_name  text NOT NULL DEFAULT '',

    -- Reserved scoping columns, single-tenant defaults in v1 — the same shape
    -- every other resource carries. Sessions are not bound to an end user
    -- (design principle 5), and a principal does not change that: it scopes
    -- authority for the request, never ownership of a session.
    org_id        text NOT NULL DEFAULT 'default',
    workspace_id  text NOT NULL DEFAULT 'default',
    project_id    text NOT NULL DEFAULT 'default',

    created_at    timestamptz NOT NULL DEFAULT now(),
    last_seen_at  timestamptz NOT NULL DEFAULT now(),

    -- The upsert target. Also the only index this table needs: every read is
    -- the authenticating lookup by exactly this pair.
    UNIQUE (issuer, subject)
);
