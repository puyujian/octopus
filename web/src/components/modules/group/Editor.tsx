'use client';

import { useCallback, useMemo, useState, type FormEvent } from 'react';
import { Check, ChevronDownIcon, Plus, Search, Sparkles, Trash2, List } from 'lucide-react';
import { useTranslations } from 'next-intl';
import * as AccordionPrimitive from '@radix-ui/react-accordion';
import { useModelChannelList, type LLMChannel } from '@/api/endpoints/model';
import { Button } from '@/components/ui/button';
import { Field, FieldGroup, FieldLabel } from '@/components/ui/field';
import { Input } from '@/components/ui/input';
import { Switch } from '@/components/ui/switch';
import { Accordion, AccordionContent, AccordionItem } from '@/components/ui/accordion';
import { cn } from '@/lib/utils';
import { getModelIcon } from '@/lib/model-icons';
import type { GroupMode } from '@/api/endpoints/group';
import type { SelectedMember } from './ItemList';
import { MemberList } from './ItemList';
import { matchesGroupName, memberKey, normalizeKey, MODE_LABELS } from './utils';
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from '@/components/animate-ui/components/animate/tooltip';
import { HelpCircle } from 'lucide-react';



export type GroupEditorValues = {
    name: string;
    match_regex: string;
    param_override: string;
    mode: GroupMode;
    first_token_time_out: number;
    session_keep_time: number;
    retry_enabled: boolean;
    max_retries: number;
    members: SelectedMember[];
};

type ParamOverrideRow = {
    id: string;
    key: string;
    value: string;
};

type ParamOverrideParseResult = {
    rows: ParamOverrideRow[];
    error: 'invalid' | 'objectOnly' | '';
};

type ParamOverrideSerializeResult = {
    value: string;
    error: 'nameRequired' | 'protected' | 'duplicate' | 'pathConflict' | '';
};

const protectedParamOverrideKeys = new Set(['model', 'stream', 'type']);
const commonParamOverrideKeys = [
    'temperature',
    'top_p',
    'max_tokens',
    'max_completion_tokens',
    'reasoning_effort',
    'enable_thinking',
    'thinking.budget_tokens',
    'reasoning.effort',
];

let paramOverrideRowSequence = 0;

function createParamOverrideRow(key = '', value = ''): ParamOverrideRow {
    paramOverrideRowSequence += 1;
    return { id: `param-override-${paramOverrideRowSequence}`, key, value };
}

function isPlainRecord(value: unknown): value is Record<string, unknown> {
    return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function isAutoParsedLiteral(value: string): boolean {
    if (value === 'true' || value === 'false' || value === 'null') return true;
    if (!/^-?(?:0|[1-9]\d*)(?:\.\d+)?(?:[eE][+-]?\d+)?$/.test(value)) return false;
    const parsed = Number(value);
    return Number.isFinite(parsed);
}

function formatParamOverrideValue(value: unknown): string {
    if (value === null) return 'null';
    if (typeof value === 'boolean' || typeof value === 'number') return String(value);
    if (typeof value === 'string') {
        // Quote strings that would otherwise be auto-detected as another type
        // so editing an existing override does not silently change its type.
        return isAutoParsedLiteral(value) ? JSON.stringify(value) : value;
    }
    return JSON.stringify(value);
}

function flattenParamOverrideObject(
    value: Record<string, unknown>,
    prefix = '',
    rows: ParamOverrideRow[] = [],
): ParamOverrideRow[] {
    Object.entries(value).forEach(([key, nestedValue]) => {
        const path = prefix ? `${prefix}.${key}` : key;
        if (isPlainRecord(nestedValue) && Object.keys(nestedValue).length > 0) {
            flattenParamOverrideObject(nestedValue, path, rows);
            return;
        }
        rows.push(createParamOverrideRow(path, formatParamOverrideValue(nestedValue)));
    });
    return rows;
}

function parseParamOverride(value: string): ParamOverrideParseResult {
    const trimmed = value.trim();
    if (!trimmed) return { rows: [], error: '' };

    try {
        const parsed = JSON.parse(trimmed) as unknown;
        if (!isPlainRecord(parsed)) return { rows: [], error: 'objectOnly' };
        return { rows: flattenParamOverrideObject(parsed), error: '' };
    } catch {
        return { rows: [], error: 'invalid' };
    }
}

function parseParamOverrideValue(rawValue: string): unknown {
    const trimmed = rawValue.trim();
    if (trimmed === '') return '';
    if (trimmed === 'true') return true;
    if (trimmed === 'false') return false;
    if (trimmed === 'null') return null;
    if (isAutoParsedLiteral(trimmed)) return Number(trimmed);

    // Complex values remain optional, but can still be entered in a row when
    // an API parameter needs an array or an object rather than a scalar.
    if ((trimmed.startsWith('{') && trimmed.endsWith('}')) || (trimmed.startsWith('[') && trimmed.endsWith(']'))) {
        try {
            const parsed = JSON.parse(trimmed) as unknown;
            if (Array.isArray(parsed) || isPlainRecord(parsed)) return parsed;
        } catch {
            // Treat malformed JSON-looking input as a normal string.
        }
    }

    if (trimmed.startsWith('"') && trimmed.endsWith('"')) {
        try {
            const parsed = JSON.parse(trimmed) as unknown;
            if (typeof parsed === 'string') return parsed;
        } catch {
            // Treat malformed quoted input as a normal string.
        }
    }
    return rawValue;
}

function setParamOverridePath(
    target: Record<string, unknown>,
    segments: string[],
    value: unknown,
): 'duplicate' | 'pathConflict' | '' {
    let current = target;
    for (let index = 0; index < segments.length; index += 1) {
        const segment = segments[index];
        const isLeaf = index === segments.length - 1;
        if (isLeaf) {
            if (Object.prototype.hasOwnProperty.call(current, segment)) return 'duplicate';
            current[segment] = value;
            return '';
        }

        if (Object.prototype.hasOwnProperty.call(current, segment)) {
            const nested = current[segment];
            if (!isPlainRecord(nested)) return 'pathConflict';
            current = nested;
        } else {
            const nested: Record<string, unknown> = {};
            current[segment] = nested;
            current = nested;
        }
    }
    return '';
}

function serializeParamOverride(rows: ParamOverrideRow[]): ParamOverrideSerializeResult {
    const result: Record<string, unknown> = {};
    const activeRows = rows.filter((row) => row.key.trim() || row.value.trim());

    for (const row of activeRows) {
        const key = row.key.trim();
        if (!key) return { value: '', error: 'nameRequired' };
        const segments = key.split('.').map((segment) => segment.trim());
        if (segments.some((segment) => !segment)) return { value: '', error: 'nameRequired' };
        if (protectedParamOverrideKeys.has(segments[0].toLowerCase())) {
            return { value: '', error: 'protected' };
        }
        const error = setParamOverridePath(result, segments, parseParamOverrideValue(row.value));
        if (error) return { value: '', error };
    }

    return {
        value: Object.keys(result).length > 0 ? JSON.stringify(result) : '',
        error: '',
    };
}

function ModelPickerSection({
    modelChannels,
    selectedMembers,
    onAdd,
    onAutoAdd,
    autoAddDisabled,
}: {
    modelChannels: LLMChannel[];
    selectedMembers: SelectedMember[];
    onAdd: (channel: LLMChannel) => void;
    onAutoAdd: () => void;
    autoAddDisabled: boolean;
}) {
    const t = useTranslations('group');
    const [searchKeyword, setSearchKeyword] = useState('');
    const [openChannels, setOpenChannels] = useState<string[]>([]);

    const selectedKeys = useMemo(() => new Set(selectedMembers.map(memberKey)), [selectedMembers]);
    const normalizedSearch = searchKeyword.trim().toLowerCase();

    const channels = useMemo(() => {
        const byId = new Map<number, { id: number; name: string; models: LLMChannel[] }>();
        modelChannels.forEach((mc) => {
            const existing = byId.get(mc.channel_id);
            if (existing) existing.models.push(mc);
            else byId.set(mc.channel_id, { id: mc.channel_id, name: mc.channel_name, models: [mc] });
        });

        return Array.from(byId.values())
            .map((c) => ({ ...c, models: [...c.models].sort((a, b) => a.name.localeCompare(b.name)) }))
            .sort((a, b) => a.id - b.id);
    }, [modelChannels]);

    const filteredChannels = useMemo(() => {
        if (!normalizedSearch) return channels;
        return channels.reduce<typeof channels>((acc, channel) => {
            if (channel.name.toLowerCase().includes(normalizedSearch)) {
                acc.push(channel);
                return acc;
            }

            const models = channel.models.filter((model) => model.name.toLowerCase().includes(normalizedSearch));
            if (models.length > 0) acc.push({ ...channel, models });
            return acc;
        }, []);
    }, [channels, normalizedSearch]);

    const hasSearch = normalizedSearch.length > 0;

    const modelSourceLabel = (model: LLMChannel) => [model.site_name, model.site_account_name, model.site_group_name]
        .map((value) => value?.trim())
        .filter(Boolean)
        .join(' / ');

    return (
        <div className="rounded-xl border border-border/50 bg-muted/30 flex flex-col min-h-0 flex-1 overflow-hidden">
            <div className="flex items-center gap-2 px-3 py-2 border-b border-border/30 bg-muted/50">
                <span className="min-w-0 shrink-0 text-sm font-medium text-foreground">
                    {t('form.addItem')}
                </span>

                <div className="relative flex-1 min-w-[120px]">
                    <Search className="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground" />
                    <Input
                        value={searchKeyword}
                        onChange={(event) => setSearchKeyword(event.target.value)}
                        className="h-7 rounded-lg border-border/60 bg-background/70 pl-8 pr-2 text-xs shadow-none focus-visible:border-border/60 focus-visible:ring-0"
                        aria-label="search"
                    />
                </div>

                <button
                    type="button"
                    onClick={onAutoAdd}
                    className={cn(
                        'shrink-0 flex items-center gap-1 px-2 py-1.5 rounded-lg text-xs font-medium transition-colors',
                        autoAddDisabled
                            ? 'text-muted-foreground/50 cursor-not-allowed'
                            : 'hover:bg-muted text-muted-foreground hover:text-foreground'
                    )}
                    disabled={autoAddDisabled}
                    title={t('form.autoAdd')}
                >
                    <Sparkles className="size-3.5" />
                    <span>{t('form.autoAdd')}</span>
                </button>
            </div>

            <div className="flex-1 min-h-0 overflow-y-auto p-2 overscroll-contain">
                <Accordion
                    type="multiple"
                    className="w-full space-y-2"
                    value={hasSearch ? filteredChannels.map((c) => `channel-${c.id}`) : openChannels}
                    onValueChange={(v) => hasSearch ? undefined : setOpenChannels(v)}
                >
                    {filteredChannels.map((channel) => {
                        const total = channel.models.length;
                        const selectedCount = channel.models.reduce(
                            (acc, m) => acc + (selectedKeys.has(memberKey(m)) ? 1 : 0),
                            0
                        );
                        const available = total - selectedCount;
                        const channelValue = `channel-${channel.id}`;
                        const isOpen = hasSearch || openChannels.includes(channelValue);

                        return (
                            <AccordionItem key={channel.id} value={channelValue}>
                                <AccordionPrimitive.Header className="rounded-lg bg-muted sticky top-0 z-10 flex px-2 overflow-hidden">
                                    <AccordionPrimitive.Trigger className="flex flex-1 min-w-0 items-center gap-4 py-4 text-left text-sm transition-all outline-none focus-visible:ring-[3px] disabled:pointer-events-none disabled:opacity-50 [&[data-state=open]>svg]:rotate-180">
                                        <span className="truncate">{channel.name}</span>
                                        <span className="text-xs text-muted-foreground shrink-0">
                                            {available}/{total}
                                        </span>
                                        <ChevronDownIcon className="text-muted-foreground pointer-events-none size-4 shrink-0 transition-transform duration-200" />
                                    </AccordionPrimitive.Trigger>
                                </AccordionPrimitive.Header>
                                <AccordionContent className="px-2 pt-2">
                                    {isOpen && (
                                    <div className="flex flex-col gap-1.5">
                                        {channel.models.map((m) => {
                                            const isSelected = selectedKeys.has(memberKey(m));
                                            const { Avatar } = getModelIcon(m.name);
                                            const sourceLabel = modelSourceLabel(m);
                                            const isSiteChannel = m.site_id != null;
                                            const suffix = [sourceLabel, isSiteChannel ? null : m.endpoint_type?.trim()]
                                                .filter(Boolean)
                                                .join(' · ');
                                            return (
                                                <button
                                                    key={memberKey(m)}
                                                    type="button"
                                                    onClick={() => !isSelected && onAdd(m)}
                                                    disabled={isSelected}
                                                    className={cn(
                                                        'w-full flex items-center justify-between gap-2 rounded-lg border border-border/50 bg-background px-2.5 py-2 text-left transition-colors',
                                                        isSelected ? 'opacity-60 cursor-not-allowed' : 'hover:bg-muted'
                                                    )}
                                                >
                                                    <span className="flex items-center gap-2 min-w-0">
                                                        <Avatar size={16} />
                                                        <span className="min-w-0 flex flex-col">
                                                            <span className="text-sm font-medium truncate">{m.name}</span>
                                                            {suffix && <span className="text-[10px] text-muted-foreground truncate">{suffix}</span>}
                                                        </span>
                                                    </span>

                                                    <span className="shrink-0 text-muted-foreground">
                                                        {isSelected ? (
                                                            <Check className="size-4 text-primary" />
                                                        ) : (
                                                            <Plus className="size-4" />
                                                        )}
                                                    </span>
                                                </button>
                                            );
                                        })}
                                    </div>
                                    )}
                                </AccordionContent>
                            </AccordionItem>
                        );
                    })}
                </Accordion>
            </div>
        </div>
    );
}

function SortSection({
    members,
    onReorder,
    onRemove,
    onWeightChange,
    removingIds,
    showWeight,
    onClear,
}: {
    members: SelectedMember[];
    onReorder: (members: SelectedMember[]) => void;
    onRemove: (id: string) => void;
    onWeightChange: (id: string, weight: number) => void;
    removingIds: Set<string>;
    showWeight: boolean;
    onClear: () => void;
}) {
    const t = useTranslations('group');

    return (
        <div className="rounded-xl border border-border/50 bg-muted/30 flex flex-col min-h-0 flex-1 overflow-hidden">
            <div className="flex items-center justify-between px-3 py-2 border-b border-border/30 bg-muted/50">
                <span className="text-sm font-medium text-foreground">
                    {t('form.items')}
                    {members.length > 0 && (
                        <span className="ml-1.5 text-xs text-muted-foreground font-normal">
                            ({members.length})
                        </span>
                    )}
                </span>
                <button
                    type="button"
                    onClick={onClear}
                    disabled={members.length === 0}
                    className={cn(
                        'flex items-center gap-1 px-2 py-1 rounded-lg text-xs font-medium transition-colors',
                        members.length === 0
                            ? 'text-muted-foreground/50 cursor-not-allowed'
                            : 'hover:bg-muted text-muted-foreground hover:text-foreground'
                    )}
                    title={t('form.clear')}
                >
                    <Trash2 className="size-3.5" />
                    <span>{t('form.clear')}</span>
                </button>
            </div>

            <div className="flex-1 min-h-0">
                <MemberList
                    members={members}
                    onReorder={onReorder}
                    onRemove={onRemove}
                    onWeightChange={onWeightChange}
                    removingIds={removingIds}
                    showWeight={showWeight}
                    showConfirmDelete={false}
                />
            </div>
        </div>
    );
}

export function GroupEditor({
    initial,
    submitText,
    submittingText,
    isSubmitting,
    onSubmit,
    onCancel,
    nameLabel,
}: {
    initial?: Partial<GroupEditorValues>;
    submitText: string;
    submittingText: string;
    isSubmitting: boolean;
    onSubmit: (values: GroupEditorValues) => void;
    onCancel?: () => void;
    nameLabel?: string;
}) {
    const t = useTranslations('group');
    const { data: modelChannels = [] } = useModelChannelList();

    const [groupName, setGroupName] = useState(initial?.name ?? '');
    const [matchRegex, setMatchRegex] = useState(initial?.match_regex ?? '');
    const [paramOverrideRows, setParamOverrideRows] = useState<ParamOverrideRow[]>(() => (
        parseParamOverride(initial?.param_override ?? '').rows
    ));
    const [paramOverrideLoadError, setParamOverrideLoadError] = useState<ParamOverrideParseResult['error']>(() => (
        parseParamOverride(initial?.param_override ?? '').error
    ));
    const [mode, setMode] = useState<GroupMode>((initial?.mode ?? 1) as GroupMode);
    const [firstTokenTimeOut, setFirstTokenTimeOut] = useState<number>(initial?.first_token_time_out ?? 0);
    const [sessionKeepTime, setSessionKeepTime] = useState<number>(initial?.session_keep_time ?? 0);
    const [retryEnabled, setRetryEnabled] = useState<boolean>(initial?.retry_enabled ?? false);
    const [maxRetries, setMaxRetries] = useState<number>(initial?.max_retries ?? 3);
    const [selectedMembers, setSelectedMembers] = useState<SelectedMember[]>(initial?.members ?? []);
    const [removingIds, setRemovingIds] = useState<Set<string>>(new Set());
    const [mobileTab, setMobileTab] = useState<string>('picker');

    const groupKey = normalizeKey(groupName);
    const regexKey = matchRegex.trim();

    const { matchedModelChannels, regexError } = useMemo(() => {
        const parseRegex = (input: string): RegExp => {
            const inlineMatch = input.match(/^\(\?([ism]+)\)(.+)$/);
            if (inlineMatch) {
                const flagMap: Record<string, string> = { i: 'i', s: 's', m: 'm' };
                const flags = inlineMatch[1].split('').map(f => flagMap[f] || '').join('');
                return new RegExp(inlineMatch[2], flags);
            }

            return new RegExp(input);
        };

        if (regexKey) {
            try {
                const re = parseRegex(regexKey);
                return { matchedModelChannels: modelChannels.filter((mc) => re.test(mc.name)), regexError: '' };
            } catch (e) {
                return { matchedModelChannels: [], regexError: (e as Error)?.message ?? 'Invalid regex' };
            }
        }
        if (!groupKey) return { matchedModelChannels: [], regexError: '' };
        return { matchedModelChannels: modelChannels.filter((mc) => matchesGroupName(mc.name, groupKey)), regexError: '' };
    }, [groupKey, regexKey, modelChannels]);

    const handleAddMember = useCallback((channel: LLMChannel) => {
        const key = memberKey(channel);
        setSelectedMembers((prev) => {
            if (prev.some((m) => m.id === key)) return prev;
            return [...prev, { ...channel, id: key, weight: 1 }];
        });
    }, []);

    const autoAddDisabled = useMemo(() => {
        if ((!regexKey && !groupKey) || regexError || matchedModelChannels.length === 0) return true;
        const existing = new Set(selectedMembers.map((m) => m.id));
        return matchedModelChannels.every((mc) => existing.has(memberKey(mc)));
    }, [groupKey, regexKey, regexError, matchedModelChannels, selectedMembers]);

    const handleAutoAdd = useCallback(() => {
        if (matchedModelChannels.length === 0) return;
        setSelectedMembers((prev) => {
            const existing = new Set(prev.map((m) => m.id));
            const toAdd = matchedModelChannels
                .filter((mc) => !existing.has(memberKey(mc)))
                .map((mc) => ({ ...mc, id: memberKey(mc), weight: 1 }));
            return toAdd.length ? [...prev, ...toAdd] : prev;
        });
    }, [matchedModelChannels]);

    const handleWeightChange = useCallback((id: string, weight: number) => {
        setSelectedMembers((prev) => prev.map((m) => m.id === id ? { ...m, weight } : m));
    }, []);

    const handleRemoveMember = useCallback((id: string) => {
        setRemovingIds((prev) => new Set(prev).add(id));
        setTimeout(() => {
            setSelectedMembers((prev) => prev.filter((m) => m.id !== id));
            setRemovingIds((prev) => { const n = new Set(prev); n.delete(id); return n; });
        }, 200);
    }, []);

    const handleClearMembers = useCallback(() => {
        setSelectedMembers([]);
        setRemovingIds(new Set());
    }, []);

    const handleAddParamOverrideRow = useCallback(() => {
        setParamOverrideLoadError('');
        setParamOverrideRows((prev) => [...prev, createParamOverrideRow()]);
    }, []);

    const handleUpdateParamOverrideRow = useCallback((id: string, field: 'key' | 'value', value: string) => {
        setParamOverrideLoadError('');
        setParamOverrideRows((prev) => prev.map((row) => (
            row.id === id ? { ...row, [field]: value } : row
        )));
    }, []);

    const handleRemoveParamOverrideRow = useCallback((id: string) => {
        setParamOverrideLoadError('');
        setParamOverrideRows((prev) => prev.filter((row) => row.id !== id));
    }, []);

    const serializedParamOverride = useMemo(
        () => serializeParamOverride(paramOverrideRows),
        [paramOverrideRows],
    );

    const paramOverrideError = useMemo(() => {
        if (paramOverrideLoadError === 'invalid') return t('form.paramOverrideInvalid');
        if (paramOverrideLoadError === 'objectOnly') return t('form.paramOverrideObjectOnly');
        switch (serializedParamOverride.error) {
            case 'nameRequired':
                return t('form.paramOverrideNameRequired');
            case 'protected':
                return t('form.paramOverrideProtected');
            case 'duplicate':
                return t('form.paramOverrideDuplicate');
            case 'pathConflict':
                return t('form.paramOverridePathConflict');
            default:
                return '';
        }
    }, [paramOverrideLoadError, serializedParamOverride.error, t]);

    const isValid = groupKey.length > 0 && selectedMembers.length > 0 && !regexError && !paramOverrideError;

    const handleSubmit = (event: FormEvent<HTMLFormElement>) => {
        event.preventDefault();
        if (!isValid) return;
        onSubmit({
            name: groupName,
            match_regex: regexKey,
            param_override: serializedParamOverride.value,
            mode,
            first_token_time_out: firstTokenTimeOut,
            session_keep_time: sessionKeepTime,
            retry_enabled: retryEnabled,
            max_retries: maxRetries,
            members: selectedMembers,
        });
    };


    return (
        <form onSubmit={handleSubmit} className="flex min-h-0 flex-col md:h-full md:overflow-hidden">
            <div className="flex-none overflow-visible px-1 md:flex-1 md:min-h-0 md:overflow-hidden">
                <FieldGroup className="flex flex-col gap-4 md:h-full md:min-h-0">
                    <div className="grid grid-cols-2 md:grid-cols-2 lg:grid-cols-4 gap-4">
                        <Field>
                            <FieldLabel htmlFor="group-name">{nameLabel ?? t('form.name')}</FieldLabel>
                            <Input
                                id="group-name"
                                value={groupName}
                                onChange={(e) => setGroupName(e.target.value)}
                                className="rounded-xl"
                            />
                        </Field>
                        <Field>
                            <FieldLabel htmlFor="group-match-regex">{t('form.matchRegex')}</FieldLabel>
                            <Input
                                id="group-match-regex"
                                value={matchRegex}
                                onChange={(e) => setMatchRegex(e.target.value)}
                                className="rounded-xl"
                                placeholder={t('form.matchRegexPlaceholder')}
                            />
                            {regexError && (
                                <p className="mt-1 text-xs text-destructive">
                                    {t('form.matchRegexInvalid')}: {regexError}
                                </p>
                            )}
                        </Field>

                        <Field>
                            <FieldLabel htmlFor="group-first-token-time-out">
                                {t('form.firstTokenTimeOut')}
                                <TooltipProvider>
                                    <Tooltip>
                                        <TooltipTrigger asChild>
                                            <HelpCircle className="size-4 text-muted-foreground cursor-help" />
                                        </TooltipTrigger>
                                        <TooltipContent>
                                            {t('form.firstTokenTimeOutHint')}
                                        </TooltipContent>
                                    </Tooltip>
                                </TooltipProvider>
                            </FieldLabel>
                            <Input
                                id="group-first-token-time-out"
                                type="number"
                                inputMode="numeric"
                                min={0}
                                step={1}
                                value={String(firstTokenTimeOut)}
                                onChange={(e) => {
                                    const raw = e.target.value;
                                    if (raw.trim() === '') {
                                        setFirstTokenTimeOut(0);
                                        return;
                                    }
                                    const n = Number.parseInt(raw, 10);
                                    setFirstTokenTimeOut(Number.isFinite(n) && n > 0 ? n : 0);
                                }}
                                className="rounded-xl"
                            />
                        </Field>

                        <Field>
                            <FieldLabel htmlFor="group-session-keep-time">
                                {t('form.sessionKeepTime')}
                                <TooltipProvider>
                                    <Tooltip>
                                        <TooltipTrigger asChild>
                                            <HelpCircle className="size-4 text-muted-foreground cursor-help" />
                                        </TooltipTrigger>
                                        <TooltipContent>
                                            {t('form.sessionKeepTimeHint')}
                                        </TooltipContent>
                                    </Tooltip>
                                </TooltipProvider>
                            </FieldLabel>
                            <Input
                                id="group-session-keep-time"
                                type="number"
                                inputMode="numeric"
                                min={0}
                                step={1}
                                value={String(sessionKeepTime)}
                                onChange={(e) => {
                                    const raw = e.target.value;
                                    if (raw.trim() === '') {
                                        setSessionKeepTime(0);
                                        return;
                                    }
                                    const n = Number.parseInt(raw, 10);
                                    setSessionKeepTime(Number.isFinite(n) && n > 0 ? n : 0);
                                }}
                                className="rounded-xl"
                            />
                        </Field>
                    </div>

                    <Field className="shrink-0">
                        <div className="flex items-center justify-between gap-2">
                            <FieldLabel htmlFor="group-param-override-key">{t('form.paramOverride')}</FieldLabel>
                            <button
                                type="button"
                                onClick={handleAddParamOverrideRow}
                                className="flex items-center gap-1 rounded-lg px-2 py-1 text-xs font-medium text-primary transition-colors hover:bg-primary/10"
                            >
                                <Plus className="size-3.5" />
                                {t('form.paramOverrideAdd')}
                            </button>
                        </div>
                        <div className="rounded-xl border border-input bg-background p-2">
                            {paramOverrideRows.length === 0 ? (
                                <p className="px-1 py-2 text-xs text-muted-foreground">
                                    {t('form.paramOverrideEmpty')}
                                </p>
                            ) : (
                                <div className="space-y-2">
                                    <div className="grid grid-cols-[minmax(0,1fr)_minmax(0,1fr)_2rem] gap-2 px-1 text-[11px] font-medium text-muted-foreground">
                                        <span>{t('form.paramOverrideName')}</span>
                                        <span>{t('form.paramOverrideValue')}</span>
                                        <span className="sr-only">{t('form.paramOverrideRemove')}</span>
                                    </div>
                                    {paramOverrideRows.map((row) => (
                                        <div key={row.id} className="grid grid-cols-[minmax(0,1fr)_minmax(0,1fr)_2rem] items-center gap-2">
                                            <Input
                                                id={row.id + '-key'}
                                                list="group-param-override-key-options"
                                                value={row.key}
                                                onChange={(event) => handleUpdateParamOverrideRow(row.id, 'key', event.target.value)}
                                                placeholder={t('form.paramOverrideNamePlaceholder')}
                                                aria-label={t('form.paramOverrideName')}
                                                className="h-8 rounded-lg text-xs"
                                            />
                                            <Input
                                                id={row.id + '-value'}
                                                value={row.value}
                                                onChange={(event) => handleUpdateParamOverrideRow(row.id, 'value', event.target.value)}
                                                placeholder={t('form.paramOverrideValuePlaceholder')}
                                                aria-label={t('form.paramOverrideValue')}
                                                className="h-8 rounded-lg text-xs"
                                            />
                                            <button
                                                type="button"
                                                onClick={() => handleRemoveParamOverrideRow(row.id)}
                                                className="flex size-8 items-center justify-center rounded-lg text-muted-foreground transition-colors hover:bg-destructive/10 hover:text-destructive"
                                                aria-label={t('form.paramOverrideRemove')}
                                            >
                                                <Trash2 className="size-3.5" />
                                            </button>
                                        </div>
                                    ))}
                                </div>
                            )}
                            <datalist id="group-param-override-key-options">
                                {commonParamOverrideKeys.map((key) => <option key={key} value={key} />)}
                            </datalist>
                        </div>
                        <p className="text-xs text-muted-foreground">{t('form.paramOverrideHint')}</p>
                        {paramOverrideError && <p className="text-xs text-destructive">{paramOverrideError}</p>}
                    </Field>

                    {/* Mode + Retry Toggle */}
                    <div className="flex flex-wrap items-center gap-2">
                        <div className="flex gap-1 flex-1 min-w-[200px]">
                            {([1, 2, 3, 4] as const).map((m) => (
                                <button
                                    key={m}
                                    type="button"
                                    onClick={() => setMode(m)}
                                    className={cn(
                                        'flex-1 py-1.5 text-xs rounded-lg transition-colors',
                                        mode === m ? 'bg-primary text-primary-foreground' : 'bg-muted hover:bg-muted/80'
                                    )}
                                >
                                    {t(`mode.${MODE_LABELS[m]}`)}
                                </button>
                            ))}
                        </div>
                        <TooltipProvider>
                            <Tooltip>
                                <TooltipTrigger asChild>
                                    <label className="flex items-center gap-1.5 shrink-0 cursor-pointer">
                                        <Switch
                                            checked={retryEnabled}
                                            onCheckedChange={setRetryEnabled}
                                        />
                                        <span className="text-xs text-muted-foreground">{t('form.retryEnabled')}</span>
                                    </label>
                                </TooltipTrigger>
                                <TooltipContent>
                                    {t('form.retryEnabledHint')}
                                </TooltipContent>
                            </Tooltip>
                        </TooltipProvider>
                        {retryEnabled && (
                            <TooltipProvider>
                                <Tooltip>
                                    <TooltipTrigger asChild>
                                        <label className="flex items-center gap-1.5 shrink-0">
                                            <Input
                                                type="number"
                                                inputMode="numeric"
                                                min={1}
                                                step={1}
                                                value={String(maxRetries)}
                                                onChange={(e) => {
                                                    const raw = e.target.value;
                                                    if (raw.trim() === '') { setMaxRetries(1); return; }
                                                    const n = Number.parseInt(raw, 10);
                                                    setMaxRetries(Number.isFinite(n) && n > 0 ? n : 1);
                                                }}
                                                className="w-16 h-7 rounded-lg text-xs text-center"
                                            />
                                            <span className="text-xs text-muted-foreground">{t('form.maxRetries')}</span>
                                        </label>
                                    </TooltipTrigger>
                                    <TooltipContent>
                                        {t('form.maxRetriesHint')}
                                    </TooltipContent>
                                </Tooltip>
                            </TooltipProvider>
                        )}
                    </div>

                    <div className="flex-none min-h-0 md:flex-1">
                        {/* Mobile: Tab switch */}
                        <div className="flex h-[min(55vh,32rem)] min-h-[20rem] flex-col md:hidden">
                            <div className="flex p-1 bg-muted rounded-xl mb-2 shrink-0 gap-1">
                                <button
                                    type="button"
                                    onClick={() => setMobileTab('picker')}
                                    className={cn(
                                        'flex-1 flex items-center justify-center gap-1.5 py-1.5 text-xs rounded-lg transition-colors',
                                        mobileTab === 'picker' ? 'bg-background shadow-sm font-medium' : 'text-muted-foreground'
                                    )}
                                >
                                    <Plus className="size-3.5" />
                                    {t('form.addItem')}
                                </button>
                                <button
                                    type="button"
                                    onClick={() => setMobileTab('sort')}
                                    className={cn(
                                        'flex-1 flex items-center justify-center gap-1.5 py-1.5 text-xs rounded-lg transition-colors',
                                        mobileTab === 'sort' ? 'bg-background shadow-sm font-medium' : 'text-muted-foreground'
                                    )}
                                >
                                    <List className="size-3.5" />
                                    {t('form.items')}
                                    {selectedMembers.length > 0 && (
                                        <span className="ml-0.5 px-1.5 py-0.5 rounded-full bg-primary/10 text-primary text-[10px] font-semibold">
                                            {selectedMembers.length}
                                        </span>
                                    )}
                                </button>
                            </div>
                            <div className="flex-1 min-h-0 flex flex-col overflow-hidden">
                                {mobileTab === 'picker' ? (
                                    <ModelPickerSection
                                        modelChannels={modelChannels}
                                        selectedMembers={selectedMembers}
                                        onAdd={handleAddMember}
                                        onAutoAdd={handleAutoAdd}
                                        autoAddDisabled={autoAddDisabled}
                                    />
                                ) : (
                                    <SortSection
                                        members={selectedMembers}
                                        onReorder={setSelectedMembers}
                                        onRemove={handleRemoveMember}
                                        onWeightChange={handleWeightChange}
                                        removingIds={removingIds}
                                        showWeight={mode === 4}
                                        onClear={handleClearMembers}
                                    />
                                )}
                            </div>
                        </div>

                        {/* Desktop: side-by-side */}
                        <div className="hidden md:grid md:grid-cols-2 gap-4 h-full min-h-0">
                            <ModelPickerSection
                                modelChannels={modelChannels}
                                selectedMembers={selectedMembers}
                                onAdd={handleAddMember}
                                onAutoAdd={handleAutoAdd}
                                autoAddDisabled={autoAddDisabled}
                            />
                            <SortSection
                                members={selectedMembers}
                                onReorder={setSelectedMembers}
                                onRemove={handleRemoveMember}
                                onWeightChange={handleWeightChange}
                                removingIds={removingIds}
                                showWeight={mode === 4}
                                onClear={handleClearMembers}
                            />
                        </div>
                    </div>
                </FieldGroup>
            </div>

            <div className="mt-auto shrink-0 px-1 pt-4">
                <div className="flex gap-2">
                    {onCancel && (
                        <Button type="button" variant="secondary" className="flex-1 rounded-xl h-11" onClick={onCancel}>
                            {t('detail.actions.cancel')}
                        </Button>
                    )}
                    <Button
                        type="submit"
                        disabled={!isValid || isSubmitting}
                        className="flex-1 rounded-xl h-11"
                    >
                        {isSubmitting ? submittingText : submitText}
                    </Button>
                </div>
            </div>
        </form>
    );
}
