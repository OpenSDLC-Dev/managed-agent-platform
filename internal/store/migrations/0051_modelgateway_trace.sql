-- A provider may opt in to receiving W3C trace context (docs/plan/59_model-gateway.md,
-- "Telemetry, errors, security"): only then does the gateway send traceparent upstream,
-- from its own span for the attempt, so a vendor's host learns the platform's trace ids
-- only where an operator chose to send them.
ALTER TABLE modelgateway.providers ADD COLUMN propagate_trace boolean NOT NULL DEFAULT false;
