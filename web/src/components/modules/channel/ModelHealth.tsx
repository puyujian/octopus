'use client';

import { useEffect, useMemo, useState } from 'react';
import { Activity, CheckCircle2, CircleAlert, LoaderCircle, Plus, RefreshCw, Sparkles, XCircle } from 'lucide-react';
import { useTranslations } from 'next-intl';
import {
    type ChannelModelGroupApplyItem,
    type ChannelModelGroupPreview,
    type ChannelModelHealth,
    type ChannelModelTarget,
    useApplyChannelModelGroups,
    useChannelModelGroupPreview,
    useChannelModelHealth,
    useRunChannelModelHealth,
} from '@/api/endpoints/channel-model';
import { useGroupList } from '@/api/endpoints/group';
import { toast } from '@/components/common/Toast';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog';
import { Input } from '@/components/ui/input';
import { Select, SelectContent, SelectGroup, SelectItem, SelectLabel, SelectSeparator, SelectTrigger, SelectValue } from '@/components/ui/select';
import { Switch } from '@/components/ui/switch';
import { cn } from '@/lib/utils';
import { useSyncChannelModels, useUpdateChannel } from '@/api/endpoints/channel';

const keyOf = (target: ChannelModelTarget) => `${target.channel_id}\u0000${target.model_name}`;

type SmartGroupBucket = 'precise' | 'fuzzy' | 'create';

function recommendedCandidate(item: ChannelModelGroupPreview) {
    return item.candidates.find((candidate) => !item.existing_group_ids.includes(candidate.group_id) && !item.excluded_group_ids.includes(candidate.group_id));
}

function smartGroupBucket(item: ChannelModelGroupPreview): SmartGroupBucket {
    const candidate = recommendedCandidate(item);
    if (!candidate) return 'create';
    return candidate.reason === 'fuzzy' ? 'fuzzy' : 'precise';
}

const smartGroupBucketRank: Record<SmartGroupBucket, number> = { precise: 0, fuzzy: 1, create: 2 };

function statusTone(status?: ChannelModelHealth['status']) {
    switch (status) {
        case 'success': return 'border-emerald-500/30 bg-emerald-500/10 text-emerald-700 dark:text-emerald-300';
        case 'failed':
        case 'interrupted': return 'border-destructive/30 bg-destructive/10 text-destructive';
        case 'queued':
        case 'running': return 'border-sky-500/30 bg-sky-500/10 text-sky-700 dark:text-sky-300';
        case 'stale': return 'border-amber-500/30 bg-amber-500/10 text-amber-700 dark:text-amber-300';
        default: return 'border-border bg-muted/40 text-muted-foreground';
    }
}

export function ChannelModelHealthBadge({ health }: { health?: ChannelModelHealth | null }) {
    const t = useTranslations('channelModel');
    const status = health?.status ?? 'idle';
    return (
        <Badge variant="outline" className={cn('h-6 shrink-0 px-2 text-[11px]', statusTone(health?.status))} title={health?.error_message || undefined}>
            {(status === 'queued' || status === 'running') && <LoaderCircle className="mr-1 size-3 animate-spin" />}
            {t(`status.${status}`)}
        </Badge>
    );
}

export function SmartGroupDialog({ targets, open, onOpenChange }: { targets: ChannelModelTarget[]; open: boolean; onOpenChange: (open: boolean) => void }) {
    const t = useTranslations('channelModel');
    const { data: groups = [] } = useGroupList();
    const previewQuery = useChannelModelGroupPreview(targets, open);
    const applyGroups = useApplyChannelModelGroups();
    const [selected, setSelected] = useState<Set<string>>(new Set());
    const [destinations, setDestinations] = useState<Record<string, string>>({});
    const [newGroupNames, setNewGroupNames] = useState<Record<string, string>>({});
    const [confirmingForce, setConfirmingForce] = useState(false);

    useEffect(() => {
        if (!open || !previewQuery.data) return;
        const nextSelected = new Set<string>();
        const nextDestinations: Record<string, string> = {};
        const nextNames: Record<string, string> = {};
        previewQuery.data.forEach((item) => {
            const key = keyOf(item);
            if (item.health?.status === 'success') nextSelected.add(key);
            const candidate = item.candidates.find((entry) => !item.existing_group_ids.includes(entry.group_id) && !item.excluded_group_ids.includes(entry.group_id));
            nextDestinations[key] = candidate ? String(candidate.group_id) : 'new';
            nextNames[key] = item.model_name;
        });
        const frame = window.requestAnimationFrame(() => {
            setSelected(nextSelected);
            setDestinations(nextDestinations);
            setNewGroupNames(nextNames);
            setConfirmingForce(false);
        });
        return () => window.cancelAnimationFrame(frame);
    }, [open, previewQuery.data]);

    const preview = useMemo(() => previewQuery.data ?? [], [previewQuery.data]);
    const orderedPreview = useMemo(() => [...preview].sort((left, right) => {
        const rankDiff = smartGroupBucketRank[smartGroupBucket(left)] - smartGroupBucketRank[smartGroupBucket(right)];
        return rankDiff || left.model_name.localeCompare(right.model_name);
    }), [preview]);
    const sections = useMemo(() => (['precise', 'fuzzy', 'create'] as SmartGroupBucket[]).map((bucket) => ({
        bucket,
        items: orderedPreview.filter((item) => smartGroupBucket(item) === bucket),
    })), [orderedPreview]);
    const running = preview.some((item) => item.health?.status === 'queued' || item.health?.status === 'running');
    const selectedItems = preview.filter((item) => selected.has(keyOf(item)) && destinations[keyOf(item)]);
    const forcedItems = selectedItems.filter((item) => item.health?.status !== 'success');

    const selectHealthy = () => {
        setConfirmingForce(false);
        setSelected(new Set(preview.filter((item) => item.health?.status === 'success').map(keyOf)));
    };

    const clearSelection = () => {
        setConfirmingForce(false);
        setSelected(new Set());
    };

    const submit = () => {
        if (forcedItems.length > 0 && !confirmingForce) {
            setConfirmingForce(true);
            return;
        }
        const items: ChannelModelGroupApplyItem[] = selectedItems.map((item) => {
            const key = keyOf(item);
            const destination = destinations[key];
            return {
                channel_id: item.channel_id,
                model_name: item.model_name,
                ...(destination === 'new'
                    ? { create_group_name: newGroupNames[key]?.trim() || item.model_name }
                    : { group_id: Number(destination) }),
                force_unhealthy: item.health?.status !== 'success',
            };
        });
        applyGroups.mutate(items, {
            onSuccess: (result) => {
                toast.success(t('toast.groupApplied', { added: result.added, existing: result.existing }));
                onOpenChange(false);
            },
            onError: (error) => toast.error(t('toast.groupFailed'), { description: error.message }),
        });
    };

    return (
        <Dialog open={open} onOpenChange={onOpenChange}>
            <DialogContent className="grid h-[100dvh] max-h-none max-w-none grid-rows-[auto_minmax(0,1fr)_auto_auto] gap-3 overflow-hidden rounded-none border-0 p-4 pb-[max(1rem,env(safe-area-inset-bottom))] pt-[max(1rem,env(safe-area-inset-top))] sm:h-fit sm:max-h-[88vh] sm:max-w-3xl sm:gap-4 sm:rounded-3xl sm:border sm:p-6">
                <DialogHeader className="pr-8 text-left">
                    <DialogTitle>{t('group.title')}</DialogTitle>
                    <DialogDescription>{t('group.description')}</DialogDescription>
                </DialogHeader>
                <div className="min-h-0 space-y-4 overflow-y-auto overscroll-contain pr-1">
                    {!previewQuery.isLoading ? (
                        <div className="space-y-2 rounded-2xl border border-border/70 bg-card p-3">
                            <div className="grid grid-cols-3 gap-2">
                                {sections.map(({ bucket, items }) => (
                                    <div key={bucket} className={cn(
                                        'rounded-xl border px-2 py-2 text-center',
                                        bucket === 'precise' && 'border-emerald-500/25 bg-emerald-500/10',
                                        bucket === 'fuzzy' && 'border-sky-500/25 bg-sky-500/10',
                                        bucket === 'create' && 'border-violet-500/25 bg-violet-500/10',
                                    )}>
                                        <div className="text-lg font-semibold tabular-nums">{items.length}</div>
                                        <div className="truncate text-[11px] text-muted-foreground">{t(`group.bucket.${bucket}.title`)}</div>
                                    </div>
                                ))}
                            </div>
                            <div className="flex flex-wrap items-center justify-between gap-2">
                                <span className="text-xs text-muted-foreground">{t('group.selectedCount', { count: selectedItems.length })}</span>
                                <div className="flex gap-1.5">
                                    <Button type="button" variant="outline" size="sm" className="h-7 rounded-lg px-2 text-xs" disabled={running} onClick={selectHealthy}>{t('group.selectHealthy')}</Button>
                                    <Button type="button" variant="ghost" size="sm" className="h-7 rounded-lg px-2 text-xs" disabled={running || selected.size === 0} onClick={clearSelection}>{t('group.clearSelection')}</Button>
                                </div>
                            </div>
                        </div>
                    ) : null}
                    {sections.map(({ bucket, items }) => items.length > 0 ? (
                        <section key={bucket} className="space-y-2">
                            <div className="sticky top-0 z-10 flex items-center justify-between gap-3 rounded-xl border bg-background/95 px-3 py-2 shadow-sm backdrop-blur">
                                <div className="min-w-0">
                                    <div className="flex items-center gap-2 text-sm font-semibold">
                                        {t(`group.bucket.${bucket}.title`)}
                                        <Badge variant="secondary" className="h-5 rounded-md px-1.5 text-[10px] tabular-nums">{items.length}</Badge>
                                    </div>
                                    <div className="truncate text-[11px] text-muted-foreground">{t(`group.bucket.${bucket}.description`)}</div>
                                </div>
                            </div>
                            {items.map((item) => {
                                const key = keyOf(item);
                                const destination = destinations[key] ?? '';
                                const preciseCandidates = item.candidates.filter((candidate) => candidate.reason === 'exact' || candidate.reason === 'regex');
                                const fuzzyCandidates = item.candidates.filter((candidate) => candidate.reason === 'fuzzy');
                                const candidateIDs = new Set(item.candidates.map((candidate) => candidate.group_id));
                                const otherGroups = groups.filter((group) => group.id != null && !candidateIDs.has(group.id));
                                const membershipSuffix = (groupID: number) => item.existing_group_ids.includes(groupID)
                                    ? ` · ${t('group.existing')}`
                                    : item.excluded_group_ids.includes(groupID) ? ` · ${t('group.excluded')}` : '';
                                return (
                                    <div key={key} className="rounded-2xl border border-border/70 bg-muted/20 p-3">
                                        <div className="grid grid-cols-[auto_minmax(0,1fr)] items-start gap-2">
                                            <input
                                                type="checkbox"
                                                checked={selected.has(key)}
                                                onChange={(event) => {
                                                    setConfirmingForce(false);
                                                    setSelected((current) => {
                                                        const next = new Set(current);
                                                        if (event.target.checked) next.add(key); else next.delete(key);
                                                        return next;
                                                    });
                                                }}
                                                disabled={running}
                                                className="mt-0.5 size-5 accent-primary"
                                            />
                                            <div className="flex min-w-0 flex-wrap items-center gap-1.5">
                                                <span className="min-w-0 flex-1 truncate text-sm font-medium" title={item.model_name}>{item.model_name}</span>
                                                <ChannelModelHealthBadge health={item.health} />
                                                {item.health?.duration_ms ? <span className="text-xs tabular-nums text-muted-foreground">{item.health.duration_ms}ms</span> : null}
                                            </div>
                                        </div>
                                        {item.health?.error_message ? <div className="mt-2 break-words text-xs text-destructive">{item.health.error_message}</div> : null}
                                        <div className={cn('mt-2 grid gap-2', destination === 'new' && 'sm:grid-cols-[minmax(0,1fr)_minmax(0,1fr)]')}>
                                            <Select value={destination} onValueChange={(value) => {
                                                setConfirmingForce(false);
                                                setDestinations((current) => ({ ...current, [key]: value }));
                                            }}>
                                                <SelectTrigger className="h-9 w-full rounded-xl"><SelectValue placeholder={t('group.select')} /></SelectTrigger>
                                                <SelectContent className="max-w-[calc(100vw-2rem)] rounded-xl">
                                                    {preciseCandidates.length > 0 ? (
                                                        <SelectGroup>
                                                            <SelectLabel>{t('group.bucket.precise.title')}</SelectLabel>
                                                            {preciseCandidates.map((candidate) => (
                                                                <SelectItem key={candidate.group_id} value={String(candidate.group_id)} disabled={item.existing_group_ids.includes(candidate.group_id) || item.excluded_group_ids.includes(candidate.group_id)}>
                                                                    {candidate.group_name} · {t(`group.reason.${candidate.reason}`)}{membershipSuffix(candidate.group_id)}
                                                                </SelectItem>
                                                            ))}
                                                        </SelectGroup>
                                                    ) : null}
                                                    {preciseCandidates.length > 0 && fuzzyCandidates.length > 0 ? <SelectSeparator /> : null}
                                                    {fuzzyCandidates.length > 0 ? (
                                                        <SelectGroup>
                                                            <SelectLabel>{t('group.bucket.fuzzy.title')}</SelectLabel>
                                                            {fuzzyCandidates.map((candidate) => (
                                                                <SelectItem key={candidate.group_id} value={String(candidate.group_id)} disabled={item.existing_group_ids.includes(candidate.group_id) || item.excluded_group_ids.includes(candidate.group_id)}>
                                                                    {candidate.group_name} · {t('group.reason.fuzzy')}{membershipSuffix(candidate.group_id)}
                                                                </SelectItem>
                                                            ))}
                                                        </SelectGroup>
                                                    ) : null}
                                                    {(preciseCandidates.length > 0 || fuzzyCandidates.length > 0) && otherGroups.length > 0 ? <SelectSeparator /> : null}
                                                    {otherGroups.length > 0 ? (
                                                        <SelectGroup>
                                                            <SelectLabel>{t('group.otherGroups')}</SelectLabel>
                                                            {otherGroups.map((group) => (
                                                                <SelectItem key={group.id} value={String(group.id)} disabled={item.existing_group_ids.includes(group.id!) || item.excluded_group_ids.includes(group.id!)}>
                                                                    {group.name}{membershipSuffix(group.id!)}
                                                                </SelectItem>
                                                            ))}
                                                        </SelectGroup>
                                                    ) : null}
                                                    {(preciseCandidates.length > 0 || fuzzyCandidates.length > 0 || otherGroups.length > 0) ? <SelectSeparator /> : null}
                                                    <SelectItem value="new"><Plus className="mr-1 inline size-3" />{t('group.create')}</SelectItem>
                                                </SelectContent>
                                            </Select>
                                            {destination === 'new' ? (
                                                <Input className="h-9 rounded-xl" value={newGroupNames[key] ?? item.model_name} onChange={(event) => {
                                                    setConfirmingForce(false);
                                                    setNewGroupNames((current) => ({ ...current, [key]: event.target.value }));
                                                }} />
                                            ) : null}
                                        </div>
                                    </div>
                                );
                            })}
                        </section>
                    ) : null)}
                    {previewQuery.isLoading && <div className="py-8 text-center text-sm text-muted-foreground">{t('loading')}</div>}
                </div>
                {confirmingForce && forcedItems.length > 0 ? (
                    <div className="rounded-2xl border border-amber-500/30 bg-amber-500/10 px-3 py-2 text-sm text-amber-800 dark:text-amber-200">
                        {t('group.forceWarning', { count: forcedItems.length })}
                    </div>
                ) : null}
                <DialogFooter className="grid grid-cols-2 sm:flex">
                    <Button variant="outline" className="w-full rounded-xl sm:w-auto" onClick={() => onOpenChange(false)}>{t('cancel')}</Button>
                    <Button className="w-full rounded-xl sm:w-auto" disabled={running || selectedItems.length === 0 || applyGroups.isPending} onClick={submit}>
                        {applyGroups.isPending ? <LoaderCircle className="size-4 animate-spin" /> : <Sparkles className="size-4" />}
                        {confirmingForce ? t('group.confirmForce') : t('group.apply', { count: selectedItems.length })}
                    </Button>
                </DialogFooter>
            </DialogContent>
        </Dialog>
    );
}

export function ChannelModelActionButtons({
    targets,
    compact = false,
    className,
}: {
    targets: ChannelModelTarget[];
    compact?: boolean;
    className?: string;
}) {
    const t = useTranslations('channelModel');
    const runHealth = useRunChannelModelHealth();
    const [dialogOpen, setDialogOpen] = useState(false);
    return (
        <div className={cn(compact ? 'grid min-w-0 grid-cols-2 gap-2 sm:flex sm:w-auto' : 'flex flex-wrap gap-2', className)}>
            <Button type="button" variant="outline" size="sm" className={cn('rounded-xl', compact && 'h-8 w-full min-w-0 overflow-hidden px-2 text-xs sm:w-auto')} disabled={targets.length === 0 || runHealth.isPending} onClick={() => runHealth.mutate(targets, {
                onSuccess: (result) => toast.success(t('toast.probeStarted', { count: result.count })),
                onError: (error) => toast.error(t('toast.probeFailed'), { description: error.message }),
            })}>
                <RefreshCw className={cn('size-4', runHealth.isPending && 'animate-spin')} />
                <span className="min-w-0 truncate">{t('probe')}</span>
            </Button>
            <Button type="button" size="sm" className={cn('rounded-xl', compact && 'h-8 w-full min-w-0 overflow-hidden px-2 text-xs sm:w-auto')} disabled={targets.length === 0} onClick={() => setDialogOpen(true)}>
                <Sparkles className="size-4" />
                <span className="min-w-0 truncate">{t('smartGroup')}</span>
            </Button>
            <SmartGroupDialog targets={targets} open={dialogOpen} onOpenChange={setDialogOpen} />
        </div>
    );
}

export function ChannelModelHealthPanel({ channelId, autoSync, targets }: { channelId: number; autoSync: boolean; targets: ChannelModelTarget[] }) {
    const t = useTranslations('channelModel');
    const healthQuery = useChannelModelHealth(targets);
    const runHealth = useRunChannelModelHealth();
    const syncModels = useSyncChannelModels();
    const updateChannel = useUpdateChannel();
    const [groupTarget, setGroupTarget] = useState<ChannelModelTarget | null>(null);
    const [autoSyncOverride, setAutoSyncOverride] = useState<boolean | null>(null);
    const healthByKey = useMemo(() => new Map((healthQuery.data ?? []).map((row) => [keyOf(row), row])), [healthQuery.data]);
    const healthy = targets.filter((target) => healthByKey.get(keyOf(target))?.status === 'success').length;
    const probingKey = runHealth.isPending && runHealth.variables?.length === 1 ? keyOf(runHealth.variables[0]) : null;
    const effectiveAutoSync = autoSyncOverride ?? autoSync;
    return (
        <section className="space-y-3">
            <div className="flex flex-col items-stretch gap-2 sm:flex-row sm:items-center sm:justify-between">
                <h4 className="flex items-center gap-2 text-xs font-semibold uppercase tracking-wider text-muted-foreground">
                    <Activity className="size-3.5" />{t('title')} · {healthQuery.isLoading ? t('loading') : `${healthy}/${targets.length}`}
                </h4>
                <ChannelModelActionButtons targets={targets} compact className="w-full sm:w-auto" />
            </div>
            <div className="flex flex-col gap-3 rounded-2xl border border-border/70 bg-muted/20 p-3 sm:flex-row sm:items-center sm:justify-between">
                <div className="min-w-0">
                    <div className="text-sm font-medium">{t('upstreamSync.title')}</div>
                    <div className="mt-0.5 text-xs text-muted-foreground">{t('upstreamSync.description')}</div>
                </div>
                <div className="grid shrink-0 grid-cols-2 gap-2 sm:flex sm:items-center">
                    <label className="flex h-8 items-center justify-center gap-2 rounded-xl border border-border/70 bg-background px-2 text-xs">
                        <Switch
                            checked={effectiveAutoSync}
                            disabled={updateChannel.isPending}
                            aria-label={t('upstreamSync.auto')}
                            onCheckedChange={(checked) => {
                                setAutoSyncOverride(checked);
                                updateChannel.mutate({ id: channelId, auto_sync: checked }, {
                                    onSuccess: () => toast.success(t(checked ? 'toast.autoSyncEnabled' : 'toast.autoSyncDisabled')),
                                    onError: (error) => {
                                        setAutoSyncOverride(null);
                                        toast.error(t('toast.autoSyncFailed'), { description: error.message });
                                    },
                                });
                            }}
                        />
                        <span>{t('upstreamSync.auto')}</span>
                    </label>
                    <Button
                        type="button"
                        variant="outline"
                        size="sm"
                        className="h-8 rounded-xl px-2 text-xs"
                        disabled={syncModels.isPending}
                        onClick={() => syncModels.mutate(channelId, {
                            onSuccess: (result) => toast.success(t('toast.syncSuccess', {
                                total: result.model_count,
                                added: result.added_models?.length ?? 0,
                                removed: result.removed_models?.length ?? 0,
                            })),
                            onError: (error) => toast.error(t('toast.syncFailed'), { description: error.message }),
                        })}
                    >
                        <RefreshCw className={cn('size-3.5', syncModels.isPending && 'animate-spin')} />
                        {t('upstreamSync.manual')}
                    </Button>
                </div>
            </div>
            <div className="max-h-56 space-y-1.5 overflow-y-auto overscroll-contain rounded-2xl border bg-card p-2">
                {targets.map((target) => {
                    const health = healthByKey.get(keyOf(target));
                    return (
                        <div key={keyOf(target)} className="grid grid-cols-[auto_minmax(0,1fr)] items-start gap-2 rounded-xl px-2 py-2 hover:bg-muted/40">
                            {health?.status === 'success' ? <CheckCircle2 className="mt-0.5 size-4 text-emerald-500" /> : health?.status === 'failed' ? <XCircle className="mt-0.5 size-4 text-destructive" /> : <CircleAlert className="mt-0.5 size-4 text-muted-foreground" />}
                            <div className="min-w-0">
                                <div className="flex min-w-0 flex-wrap items-center gap-1.5">
                                    <span className="min-w-0 flex-1 truncate text-sm" title={target.model_name}>{target.model_name}</span>
                                    <ChannelModelHealthBadge health={health} />
                                    {health?.duration_ms ? <span className="text-right text-xs tabular-nums text-muted-foreground">{health.duration_ms}ms</span> : null}
                                </div>
                                {health?.error_message ? <div className="mt-1 line-clamp-2 break-all text-xs text-destructive">{health.error_message}</div> : null}
                                <div className="mt-2 flex flex-wrap justify-end gap-1.5">
                                    <Button
                                        type="button"
                                        variant="outline"
                                        size="sm"
                                        className="h-7 rounded-lg px-2 text-xs"
                                        disabled={runHealth.isPending}
                                        aria-label={t('probeModel', { model: target.model_name })}
                                        title={t('probeModel', { model: target.model_name })}
                                        onClick={() => runHealth.mutate([target], {
                                            onSuccess: (result) => toast.success(t('toast.probeStarted', { count: result.count })),
                                            onError: (error) => toast.error(t('toast.probeFailed'), { description: error.message }),
                                        })}
                                    >
                                        {probingKey === keyOf(target) ? <LoaderCircle className="size-3.5 animate-spin" /> : <Activity className="size-3.5" />}
                                        {t('probe')}
                                    </Button>
                                    <Button
                                        type="button"
                                        variant="outline"
                                        size="sm"
                                        className="h-7 rounded-lg px-2 text-xs"
                                        onClick={() => setGroupTarget(target)}
                                        aria-label={t('joinModel', { model: target.model_name })}
                                        title={t('joinModel', { model: target.model_name })}
                                    >
                                        <Sparkles className="size-3.5" />
                                        {t('join')}
                                    </Button>
                                </div>
                            </div>
                        </div>
                    );
                })}
                {targets.length === 0 && <div className="py-4 text-center text-sm text-muted-foreground">{t('empty')}</div>}
            </div>
            <SmartGroupDialog targets={groupTarget ? [groupTarget] : []} open={groupTarget !== null} onOpenChange={(open) => !open && setGroupTarget(null)} />
        </section>
    );
}
