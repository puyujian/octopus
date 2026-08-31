import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { apiClient } from '../client';

export type ChannelModelTarget = { channel_id: number; model_name: string };
export type ChannelModelHealthStatus = 'queued' | 'running' | 'success' | 'failed' | 'stale' | 'interrupted';

export type ChannelModelHealth = ChannelModelTarget & {
    id: number;
    status: ChannelModelHealthStatus;
    http_status: number;
    duration_ms: number;
    error_message: string;
    channel_key_id: number;
    key_remark: string;
    checked_at?: string | null;
};

export type ChannelModelGroupPreview = ChannelModelTarget & {
    health?: ChannelModelHealth | null;
    existing_group_ids: number[];
    excluded_group_ids: number[];
    candidates: Array<{ group_id: number; group_name: string; reason: 'exact' | 'regex' | 'fuzzy' }>;
};

export type ChannelModelGroupApplyItem = ChannelModelTarget & {
    group_id?: number;
    create_group_name?: string;
    force_unhealthy?: boolean;
};

export type ChannelModelGroupApplyResult = {
    added: number;
    existing: number;
    excluded: number;
    skipped: number;
    created_groups: number;
    failed: string[];
};

function targetsKey(targets: ChannelModelTarget[]) {
    return targets.map((target) => `${target.channel_id}:${target.model_name}`).sort().join('|');
}

function invalidateChannelModels(queryClient: ReturnType<typeof useQueryClient>) {
    queryClient.invalidateQueries({ queryKey: ['channel-model-health'] });
    queryClient.invalidateQueries({ queryKey: ['channel-model-group-preview'] });
    queryClient.invalidateQueries({ queryKey: ['groups', 'list'] });
    queryClient.invalidateQueries({ queryKey: ['channels', 'list'] });
    queryClient.invalidateQueries({ queryKey: ['site-channel', 'list'] });
}

export function useChannelModelHealth(targets: ChannelModelTarget[], enabled = true) {
    const key = targetsKey(targets);
    return useQuery({
        queryKey: ['channel-model-health', key],
        queryFn: () => apiClient.post<ChannelModelHealth[]>('/api/v1/channel-model/health/query', { targets }),
        enabled: enabled && targets.length > 0,
        refetchInterval: (query) => {
            const rows = query.state.data as ChannelModelHealth[] | undefined;
            return rows?.some((row) => row.status === 'queued' || row.status === 'running') ? 2000 : 30000;
        },
    });
}

export function useRunChannelModelHealth() {
    const queryClient = useQueryClient();
    return useMutation({
        mutationFn: (targets: ChannelModelTarget[]) => apiClient.post<{ task_id: string; count: number }>('/api/v1/channel-model/health/run', { targets }),
        onSuccess: () => invalidateChannelModels(queryClient),
    });
}

export function useChannelModelGroupPreview(targets: ChannelModelTarget[], enabled: boolean) {
    const key = targetsKey(targets);
    return useQuery({
        queryKey: ['channel-model-group-preview', key],
        queryFn: () => apiClient.post<ChannelModelGroupPreview[]>('/api/v1/channel-model/group/preview', { targets }),
        enabled: enabled && targets.length > 0,
        refetchInterval: (query) => {
            const rows = query.state.data as ChannelModelGroupPreview[] | undefined;
            return rows?.some((row) => row.health?.status === 'queued' || row.health?.status === 'running') ? 2000 : false;
        },
    });
}

export function useApplyChannelModelGroups() {
    const queryClient = useQueryClient();
    return useMutation({
        mutationFn: (items: ChannelModelGroupApplyItem[]) => apiClient.post<ChannelModelGroupApplyResult>('/api/v1/channel-model/group/apply', { items }),
        onSuccess: () => invalidateChannelModels(queryClient),
    });
}
