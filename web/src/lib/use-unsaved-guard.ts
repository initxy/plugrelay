import { useBlocker } from "@tanstack/react-router";
import { useConfirm } from "@/components/confirm";
import { useI18n } from "@/i18n";

/**
 * Warn before leaving a page (in-app navigation or tab close) with unsaved edits.
 * Search-only changes on the same page (opening or closing a drawer) are not blocked;
 * drawers ask through useDiscardCheck instead.
 */
export function useUnsavedGuard(dirty: boolean): void {
  const confirm = useConfirm();
  const { t } = useI18n();
  useBlocker({
    shouldBlockFn: async ({ current, next }) => {
      if (!dirty || current.pathname === next.pathname) return false;
      const leave = await confirm({
        title: t("unsaved.title"),
        body: t("unsaved.body"),
        confirm: t("unsaved.discard"),
        danger: true,
      });
      return !leave;
    },
    enableBeforeUnload: () => dirty,
  });
}

/** For drawers and dialogs: ask before closing with unsaved edits. Resolves true when it may close. */
export function useDiscardCheck() {
  const confirm = useConfirm();
  const { t } = useI18n();
  return (dirty: boolean) =>
    dirty
      ? confirm({ title: t("unsaved.title"), body: t("unsaved.body"), confirm: t("unsaved.discard"), danger: true })
      : Promise.resolve(true);
}
