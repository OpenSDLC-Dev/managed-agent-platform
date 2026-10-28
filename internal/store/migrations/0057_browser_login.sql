-- Browser credentials are separate from agent sessions and worker/gate tokens.
-- Only hashes of browser bearer tokens and throttle keys reach these tables.
CREATE TABLE browser_sessions (
    token_hash   text PRIMARY KEY CHECK (token_hash ~ '^[0-9a-f]{64}$'),
    principal_id text NOT NULL REFERENCES principals(id),
    created_at   timestamptz NOT NULL DEFAULT clock_timestamp(),
    expires_at   timestamptz NOT NULL CHECK (expires_at > created_at)
);
CREATE INDEX browser_sessions_expiry ON browser_sessions (expires_at);

-- The local principal's issuer/subject is the login identity, never its email.
-- PHC parsing and the fixed Argon2 policy live in internal/login.
CREATE TABLE local_accounts (
    principal_id  text PRIMARY KEY REFERENCES principals(id),
    password_hash text NOT NULL CHECK (octet_length(password_hash) BETWEEN 1 AND 128)
);

-- One-time OIDC state is bound to a pending browser cookie. The code verifier
-- must survive a control-plane replica change during authorization-code login.
CREATE TABLE browser_login_attempts (
    state_hash    text PRIMARY KEY CHECK (state_hash ~ '^[0-9a-f]{64}$'),
    binding_hash  text NOT NULL CHECK (binding_hash ~ '^[0-9a-f]{64}$'),
    nonce         text NOT NULL,
    code_verifier text NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT clock_timestamp(),
    expires_at    timestamptz NOT NULL CHECK (expires_at > created_at)
);
CREATE INDEX browser_login_attempts_expiry ON browser_login_attempts (expires_at);

CREATE TABLE login_throttle (
    bucket_hash  text PRIMARY KEY CHECK (bucket_hash ~ '^[0-9a-f]{64}$'),
    window_start timestamptz NOT NULL,
    attempts     integer NOT NULL CHECK (attempts > 0)
);
CREATE INDEX login_throttle_expiry ON login_throttle (window_start);
