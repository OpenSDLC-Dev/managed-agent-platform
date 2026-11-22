-- Existing rates have no declared currency or pricing terms.
ALTER TABLE modelgateway.deployments
    ADD COLUMN price_currency text NOT NULL DEFAULT ''
        CHECK (price_currency = '' OR price_currency COLLATE "C" ~ '^[A-Z]{3}$'),
    ADD COLUMN price_note text NOT NULL DEFAULT ''
        CHECK (char_length(price_note) <= 256);
