import { cn } from "@/lib/utils";

export function BrandMark({ size = 22, className }: { size?: number; className?: string }) {
  return (
    <img src="/kiro-gateway.svg" alt="" width={size} height={size} className={cn("shrink-0", className)} />
  );
}
