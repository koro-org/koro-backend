-- A down migration never makes a [ts] table.
CREATE TRIGGER event_staff_touch BEFORE UPDATE ON event_staff
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
