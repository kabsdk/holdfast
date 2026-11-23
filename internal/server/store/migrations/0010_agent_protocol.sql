-- M5: the agent protocol each agent last reported; 0 from an agent before M5, which
-- speaks protocol 1.
ALTER TABLE agents ADD COLUMN protocol integer NOT NULL DEFAULT 0;
