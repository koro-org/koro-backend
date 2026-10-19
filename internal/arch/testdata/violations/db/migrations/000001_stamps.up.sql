-- The [ts] tables of the one-clock rule (TestArch_TsStampsFromDatabase):
-- events, event_forms and posts; event_staff is [c] (append-only).
CREATE TRIGGER events_touch_updated_at BEFORE UPDATE ON events
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
create trigger event_forms_touch before update on public.event_forms for each row execute function koro_touch_updated_at();
CREATE TRIGGER event_staff_forbid BEFORE UPDATE ON event_staff
  FOR EACH ROW WHEN (OLD.role IS DISTINCT FROM NEW.role) EXECUTE FUNCTION public.koro_forbid_mutation();
CREATE TRIGGER posts_touch_updated_at BEFORE UPDATE ON posts
  FOR EACH ROW WHEN (OLD.like_count IS NOT DISTINCT FROM NEW.like_count)
  EXECUTE FUNCTION public.koro_touch_updated_at();
