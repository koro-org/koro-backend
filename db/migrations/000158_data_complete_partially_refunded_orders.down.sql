-- 000158_data_complete_partially_refunded_orders.down.sql (BE-135). Nothing to
-- undo: the up migration only corrected the state of orders with the rule
-- the code applies (orders.progressStatus) and changed no schema object.
-- Moving the orders back to partially_refunded would bring the bug back,
-- and which ones were corrected is not recorded.
SELECT 1;
