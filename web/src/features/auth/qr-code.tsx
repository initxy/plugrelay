import qrcode from "qrcode-generator";
import { useMemo } from "react";

/** A QR code drawn as SVG modules; nothing leaves the browser. */
export function QrCode({ value, label, size = 176 }: { value: string; label: string; size?: number }) {
  const { n, path } = useMemo(() => {
    const qr = qrcode(0, "M");
    qr.addData(value);
    qr.make();
    const n = qr.getModuleCount();
    let d = "";
    for (let r = 0; r < n; r++) for (let c = 0; c < n; c++) if (qr.isDark(r, c)) d += `M${c + 4} ${r + 4}h1v1h-1z`;
    return { n: n + 8, path: d };
  }, [value]);
  return (
    <svg role="img" aria-label={label} viewBox={`0 0 ${n} ${n}`} width={size} height={size} shapeRendering="crispEdges" className="rounded-md bg-white">
      <path d={path} fill="#000" />
    </svg>
  );
}
