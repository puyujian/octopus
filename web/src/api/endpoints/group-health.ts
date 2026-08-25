import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { apiClient } from '../client';
import { logger } from '@/lib/logger';
import { useGroupHealthEnabled } from './setting';
import type { GroupMode } from './group';

export type GroupHealthStatus = 'running' | 'success' | 'partial' | 'failed';
export type GroupHealthAttemptStatus = 'success' | 'failed' | 'skipped';
export type GroupHealthProbeMode = 'standard' | 'full';
export type GroupHealthMembershipState = 'active' | 'excluded' | 'missing';

export interface GroupHealthAttempt {
    id: number;
    snapshot_id: number;
    group_item_id: number;
    channel_id: number;
    channel_name: string;
    channel_key_id: number;
    key_remark: string;
    model_name: string;
    priority: number;
    weight: number;
    status: GroupHealthAttemptStatus;
    http_status: number;
    duration_ms: number;
    error_message: string;
    membership_state: GroupHealthMembershipState;
}

export interface GroupHealthExcludedItem {
    id: number; group_id: number; channel_id: number; channel_name: string; channel_enabled: boolean;
    model_name: string; priority: number; weight: number; excluded_at?: string | null;
    excluded_by_attempt_id?: number | null; http_status: number; duration_ms: number; error_message: string;
}

export interface GroupHealthRecoveryResult {
    item_id: number; restored: boolean; active_item_count: number;
    probe: { success: boolean; http_status: number; duration_ms: number; error_message: string };
}

export interface GroupHealthBatchRecoveryResult {
    total: number;
    restored_count: number;
    failed_count: number;
    active_item_count: number;
    results: GroupHealthRecoveryResult[];
}

export interface GroupHealthSnapshot {
    id: number;
    group_id: number;
    group_name: string;
    group_mode: GroupMode;
    probe_mode: GroupHealthProbeMode;
    request_model: string;
    status: GroupHealthStatus;
    started_at: string;
    finished_at?: string | null;
    duration_ms: number;
    successful_channel_id?: number | null;
    message: string;
    attempts: GroupHealthAttempt[];
}

export interface GroupHealthGroupView {
    group_id: number;
    group_name: string;
    group_mode: GroupMode;
    active_item_count: number;
    excluded_items: GroupHealthExcludedItem[];
    latest?: GroupHealthSnapshot | null;
}

export type RunGroupHealthAccepted = {
    group_id?: number;
    all_groups?: boolean;
    probe_mode?: GroupHealthProbeMode;
};

export type RunGroupHealthRequest = {
    groupId: number;
    probeMode?: GroupHealthProbeMode;
};

export type RunAllGroupHealthRequest = {
    probeMode?: GroupHealthProbeMode;
};

function normalizeAttempt(attempt: Partial<GroupHealthAttempt>): GroupHealthAttempt {
    return {
        id: typeof attempt.id === 'number' ? attempt.id : 0,
        snapshot_id: typeof attempt.snapshot_id === 'number' ? attempt.snapshot_id : 0,
        group_item_id: typeof attempt.group_item_id === 'number' ? attempt.group_item_id : 0,
        channel_id: typeof attempt.channel_id === 'number' ? attempt.channel_id : 0,
        channel_name: attempt.channel_name ?? '',
        channel_key_id: typeof attempt.channel_key_id === 'number' ? attempt.channel_key_id : 0,
        key_remark: attempt.key_remark ?? '',
        model_name: attempt.model_name ?? '',
        priority: typeof attempt.priority === 'number' ? attempt.priority : 0,
        weight: typeof attempt.weight === 'number' ? attempt.weight : 0,
        status: attempt.status === 'success' || attempt.status === 'failed' || attempt.status === 'skipped'
            ? attempt.status
            : 'failed',
        http_status: typeof attempt.http_status === 'number' ? attempt.http_status : 0,
        duration_ms: typeof attempt.duration_ms === 'number' ? attempt.duration_ms : 0,
        error_message: attempt.error_message ?? '',
        membership_state: attempt.membership_state === 'excluded' || attempt.membership_state === 'missing' ? attempt.membership_state : 'active',
    };
}

function normalizeSnapshot(snapshot: Partial<GroupHealthSnapshot> | null | undefined): GroupHealthSnapshot | null {
    if (!snapshot) return null;
    return {
        id: typeof snapshot.id === 'number' ? snapshot.id : 0,
        group_id: typeof snapshot.group_id === 'number' ? snapshot.group_id : 0,
        group_name: snapshot.group_name ?? '',
        group_mode: typeof snapshot.group_mode === 'number' ? snapshot.group_mode : 1,
        probe_mode: snapshot.probe_mode === 'full' ? 'full' : 'standard',
        request_model: snapshot.request_model ?? '',
        status: snapshot.status === 'running' || snapshot.status === 'success' || snapshot.status === 'partial' || snapshot.status === 'failed'
            ? snapshot.status
            : 'failed',
        started_at: snapshot.started_at ?? '',
        finished_at: snapshot.finished_at ?? null,
        duration_ms: typeof snapshot.duration_ms === 'number' ? snapshot.duration_ms : 0,
        successful_channel_id: typeof snapshot.successful_channel_id === 'number' ? snapshot.successful_channel_id : null,
        message: snapshot.message ?? '',
        attempts: (snapshot.attempts ?? []).map(normalizeAttempt),
    };
}

function normalizeView(view: Partial<GroupHealthGroupView>): GroupHealthGroupView {
    return {
        group_id: typeof view.group_id === 'number' ? view.group_id : 0,
        group_name: view.group_name ?? '',
        group_mode: typeof view.group_mode === 'number' ? view.group_mode : 1,
        active_item_count: typeof view.active_item_count === 'number' ? view.active_item_count : 0,
        excluded_items: (view.excluded_items ?? []).map((item) => ({
            id: item.id ?? 0, group_id: item.group_id ?? 0, channel_id: item.channel_id ?? 0,
            channel_name: item.channel_name ?? '', channel_enabled: item.channel_enabled ?? false,
            model_name: item.model_name ?? '', priority: item.priority ?? 0, weight: item.weight ?? 0,
            excluded_at: item.excluded_at ?? null, excluded_by_attempt_id: item.excluded_by_attempt_id ?? null,
            http_status: item.http_status ?? 0, duration_ms: item.duration_ms ?? 0, error_message: item.error_message ?? '',
        })),
        latest: normalizeSnapshot(view.latest),
    };
}

function invalidateGroupHealth(queryClient: ReturnType<typeof useQueryClient>) {
    queryClient.invalidateQueries({ queryKey: ['group-health', 'list'] });
    queryClient.invalidateQueries({ queryKey: ['groups', 'list'] });
    queryClient.invalidateQueries({ queryKey: ['group-health', 'detail'] });
}

export function useExcludeGroupHealthAttempt() {
    const queryClient = useQueryClient();
    return useMutation({
        mutationFn: ({ groupId, attemptId, allowEmpty = false }: { groupId: number; attemptId: number; allowEmpty?: boolean }) =>
            apiClient.post<GroupHealthGroupView>(`/api/v1/group/health/${groupId}/attempts/${attemptId}/exclude`, { allow_empty: allowEmpty }),
        onSuccess: () => invalidateGroupHealth(queryClient),
        onError: (error) => logger.error('group health exclusion failed:', error),
    });
}

export function useExcludeAllFailedGroupHealthAttempts() {
    const queryClient = useQueryClient();
    return useMutation({
        mutationFn: ({ groupId, allowEmpty = false }: { groupId: number; allowEmpty?: boolean }) =>
            apiClient.post<GroupHealthGroupView>(`/api/v1/group/health/${groupId}/attempts/exclude-failed`, { allow_empty: allowEmpty }),
        onSuccess: () => invalidateGroupHealth(queryClient),
        onError: (error) => logger.error('batch group health exclusion failed:', error),
    });
}

export function useProbeAndRestoreGroupHealthItem() {
    const queryClient = useQueryClient();
    return useMutation({
        mutationFn: ({ groupId, itemId }: { groupId: number; itemId: number }) =>
            apiClient.post<GroupHealthRecoveryResult>(`/api/v1/group/health/${groupId}/items/${itemId}/probe-and-restore`, {}),
        onSuccess: () => invalidateGroupHealth(queryClient),
    });
}

export function useProbeAndRestoreAllGroupHealthItems() {
    const queryClient = useQueryClient();
    return useMutation({
        mutationFn: ({ groupId }: { groupId: number }) =>
            apiClient.post<GroupHealthBatchRecoveryResult>(`/api/v1/group/health/${groupId}/items/probe-and-restore`, {}),
        onSuccess: () => invalidateGroupHealth(queryClient),
    });
}

export function useForceRestoreGroupHealthItem() {
    const queryClient = useQueryClient();
    return useMutation({
        mutationFn: ({ groupId, itemId }: { groupId: number; itemId: number }) =>
            apiClient.post<GroupHealthRecoveryResult>(`/api/v1/group/health/${groupId}/items/${itemId}/restore`, {}),
        onSuccess: () => invalidateGroupHealth(queryClient),
    });
}

export function useGroupHealthList() {
    const { enabled } = useGroupHealthEnabled();
    return useQuery({
        queryKey: ['group-health', 'list'],
        queryFn: async () => apiClient.get<GroupHealthGroupView[]>('/api/v1/group/health/list'),
        select: (data) => data.map(normalizeView),
        enabled,
        refetchInterval: (query) => {
            const data = query.state.data as GroupHealthGroupView[] | undefined;
            return data?.some((item) => item.latest?.status === 'running') ? 5000 : 30000;
        },
    });
}

export function useGroupHealth(groupId: number | null) {
    const { enabled } = useGroupHealthEnabled();
    return useQuery({
        queryKey: ['group-health', 'detail', groupId],
        queryFn: async () => apiClient.get<GroupHealthGroupView>(`/api/v1/group/health/${groupId}`),
        select: normalizeView,
        enabled: enabled && groupId != null && groupId > 0,
        refetchInterval: (query) => {
            const data = query.state.data as GroupHealthGroupView | undefined;
            return data?.latest?.status === 'running' ? 5000 : 30000;
        },
    });
}

export function useRunGroupHealth() {
    const queryClient = useQueryClient();
    const { enabled } = useGroupHealthEnabled();
    return useMutation({
        mutationFn: async ({ groupId, probeMode }: RunGroupHealthRequest) => {
            if (!enabled) throw new Error('Group health checks are disabled');
            return apiClient.post<RunGroupHealthAccepted>(
                `/api/v1/group/health/${groupId}/run`,
                probeMode ? { probe_mode: probeMode } : {},
            );
        },
        onSuccess: () => invalidateGroupHealth(queryClient),
        onError: (error) => logger.error('group health run failed:', error),
    });
}

export function useRunAllGroupHealth() {
    const queryClient = useQueryClient();
    const { enabled } = useGroupHealthEnabled();
    return useMutation({
        mutationFn: async ({ probeMode }: RunAllGroupHealthRequest = {}) => {
            if (!enabled) throw new Error('Group health checks are disabled');
            return apiClient.post<RunGroupHealthAccepted>(
                '/api/v1/group/health/run-all',
                probeMode ? { probe_mode: probeMode } : {},
            );
        },
        onSuccess: () => invalidateGroupHealth(queryClient),
        onError: (error) => logger.error('group health run-all failed:', error),
    });
}
