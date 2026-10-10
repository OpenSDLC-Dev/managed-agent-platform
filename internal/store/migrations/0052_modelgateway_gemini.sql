-- The Gemini API joins the model gateway's upstream protocols
-- (docs/plan/62_gemini-upstream-protocol.md): a provider may have a Gemini
-- endpoint, alone or beside the other two, and a credential may be usable on it.
ALTER TABLE modelgateway.providers ADD COLUMN gemini_base_url text;

ALTER TABLE modelgateway.providers DROP CONSTRAINT providers_check;
ALTER TABLE modelgateway.providers ADD CONSTRAINT providers_endpoint_check
    CHECK (anthropic_base_url IS NOT NULL OR openai_base_url IS NOT NULL OR gemini_base_url IS NOT NULL);

ALTER TABLE modelgateway.credentials DROP CONSTRAINT credentials_protocols_check;
ALTER TABLE modelgateway.credentials ADD CONSTRAINT credentials_protocols_check
    CHECK (cardinality(protocols) > 0 AND protocols <@ ARRAY['anthropic', 'openai', 'gemini']);
