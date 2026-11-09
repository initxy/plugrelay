/** Placeholder to pass as a message param where `WithRawValue` puts the value. */
export const RAW_SLOT = "\u0000";

/**
 * A translated sentence around a value shown as typed (a model id, a name from the upstream),
 * which may look like a config key but is data: `text` holds `RAW_SLOT` where `value` goes.
 */
export function WithRawValue({ text, value }: { text: string; value: string }) {
  const [before, after = ""] = text.split(RAW_SLOT);
  return (
    <>
      {before}
      <span data-raw className="font-mono">
        {value}
      </span>
      {after}
    </>
  );
}
