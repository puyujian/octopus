'use client';

import { useMemo } from 'react';
import { useTranslations } from 'next-intl';
import { toast } from 'sonner';
import { useGroupList, useUpdateGroup } from '@/api/endpoints/group';
import type { GroupHealthGroupView } from '@/api/endpoints/group-health';

export function useGroupHealthReorder(view?: GroupHealthGroupView, disabled = false) {
    const t = useTranslations('group.health');
    const { data: groups = [] } = useGroupList();
    const updateGroup = useUpdateGroup();
    const group = groups.find((item) => item.id === view?.group_id);

    const { attempts, updates, hasSpeedResults, itemCount } = useMemo(() => {
        const items = (group?.items ?? []).filter((item) => item.id != null);
        const itemByID = new Map(items.map((item) => [item.id, item]));
        const speeds = new Map<number, number>();
        const attempts = view?.latest?.attempts ?? [];
        for (const attempt of attempts) {
            const item = itemByID.get(attempt.group_item_id);
            // Only use successful timings for the same current channel/model member.
            // Skipped checks and fast failures must never become the fastest candidates.
            if (item && item.channel_id === attempt.channel_id && item.model_name === attempt.model_name
                && attempt.membership_state === 'active' && attempt.status === 'success'
                && Number.isFinite(attempt.duration_ms) && attempt.duration_ms > 0) {
                speeds.set(attempt.group_item_id, attempt.duration_ms);
            }
        }
        const sortedItems = [...items].sort((left, right) => {
            const leftSpeed = speeds.get(left.id!) ?? Infinity;
            const rightSpeed = speeds.get(right.id!) ?? Infinity;
            return (leftSpeed - rightSpeed) || left.priority - right.priority;
        });
        const updates = sortedItems.flatMap((item, index) => item.priority === index + 1 ? [] : [{
            id: item.id!, priority: index + 1, weight: item.weight,
        }]);
        // Display current member order while keeping the original health snapshot intact.
        const orderedAttempts = [...attempts].sort((left, right) => {
            const leftPriority = itemByID.get(left.group_item_id)?.priority ?? Infinity;
            const rightPriority = itemByID.get(right.group_item_id)?.priority ?? Infinity;
            return (leftPriority - rightPriority) || left.priority - right.priority;
        });
        return { attempts: orderedAttempts, updates, hasSpeedResults: speeds.size > 0, itemCount: items.length };
    }, [group?.items, view?.latest?.attempts]);

    const canReorder = Boolean(view && group && itemCount > 1 && hasSpeedResults
        && view.latest?.status !== 'running' && !disabled && !updateGroup.isPending);

    const reorder = () => {
        if (!canReorder || !view) return;
        if (updates.length === 0) {
            toast.success(t('reorderUnchanged'));
            return;
        }
        updateGroup.mutate({ id: view.group_id, items_to_update: updates }, {
            onSuccess: () => toast.success(t('reorderSuccess')),
            onError: (error) => toast.error(t('reorderFailed'), { description: error.message }),
        });
    };

    return { attempts, canReorder, hasSpeedResults, isPending: updateGroup.isPending, reorder };
}
