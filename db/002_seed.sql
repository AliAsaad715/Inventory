-- Stock for the app's demo products: the same fixed IDs as the app's product seed, in the
-- pattern of its seeded categories. Product X has a single unit, so the interview demo's 20
-- simultaneous orders for it end with exactly one accepted; the running shoes start sold out,
-- for the not-enough-stock answer. Products added later through the app have no row here and
-- count as 0 available until a SetStock RPC exists.
BEGIN;

INSERT INTO stock_levels (product_id, available) VALUES
    ('019a2180-0000-7000-8000-000000000201', 25),  -- Wireless Headphones
    ('019a2180-0000-7000-8000-000000000202', 100), -- Cotton T-Shirt
    ('019a2180-0000-7000-8000-000000000203', 40),  -- Ceramic Mug
    ('019a2180-0000-7000-8000-000000000204', 15),  -- Field Guide to Distributed Systems
    ('019a2180-0000-7000-8000-000000000205', 1),   -- Product X: Limited Edition Desk Lamp
    ('019a2180-0000-7000-8000-000000000206', 0)    -- Trail Running Shoes, sold out
-- Loading the seed twice changes nothing and never resets stock that orders have moved
ON CONFLICT (product_id) DO NOTHING;

COMMIT;
