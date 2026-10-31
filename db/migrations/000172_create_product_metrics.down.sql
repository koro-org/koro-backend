-- 000172_create_product_metrics.down.sql: the product KPI functions, the
-- daily snapshot (its rows are aggregates only) and their analytics views go.
BEGIN;
SET LOCAL lock_timeout = '5s';
DROP VIEW analytics.product_metrics_daily;
DROP VIEW analytics.product_metrics_current;
DROP VIEW analytics.product_metrics_monthly;
DROP TABLE product_metrics_daily;
DROP FUNCTION public.koro_metrics_current(timestamptz);
DROP FUNCTION public.koro_metrics_period(timestamptz, timestamptz);
DROP FUNCTION public.koro_student_verified_at(uuid, timestamptz);
DROP FUNCTION public.koro_metric(text, text, bigint, bigint, boolean, boolean);
COMMIT;
