-- Organization-wide limits for the public model (configured alias). All
-- wildcard matches share '*'. No key, protocol or gateway replica isolates
-- the budget. Keep limits when an alias is deleted and recreated.
CREATE TABLE modelgateway.model_limits (
    model text PRIMARY KEY CHECK (model <> '' AND char_length(model) <= 256),
    rpm bigint CHECK (rpm > 0),
    itpm bigint CHECK (itpm > 0),
    otpm bigint CHECK (otpm > 0)
);
CREATE TABLE modelgateway.model_rate_windows (
    model text NOT NULL,
    minute timestamptz NOT NULL,
    requests bigint NOT NULL DEFAULT 0,
    input_tokens bigint NOT NULL DEFAULT 0,
    output_tokens bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (model, minute)
);
