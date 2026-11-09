import { useQuery } from "@tanstack/react-query";
import { api, unwrap } from "@/lib/api/client";

export const useModels = () => useQuery({ queryKey: ["models"], queryFn: () => unwrap(api.GET("/models")) });
export const usePresets = () => useQuery({ queryKey: ["presets"], queryFn: () => unwrap(api.GET("/presets")) });
export const usePricing = () => useQuery({ queryKey: ["pricing-rules"], queryFn: () => unwrap(api.GET("/pricing-rules")) });
export const useGroups = () => useQuery({ queryKey: ["groups"], queryFn: () => unwrap(api.GET("/groups")) });
