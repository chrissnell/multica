"use client";

import { useMemo } from "react";
import { useQuery } from "@tanstack/react-query";
import { Activity } from "lucide-react";
import { Skeleton } from "@multica/ui/components/ui/skeleton";
import { useWorkspaceId } from "@multica/core/hooks";
import { agentTaskSnapshotOptions } from "@multica/core/agents";
import { PageHeader } from "../../layout/page-header";
import { useT } from "../../i18n";
import { ActiveIssueCard } from "./active-issue-card";

/**
 * "Active Issues" — a live grid of every issue that currently has at least
 * one agent in the `running` state. Derived entirely from the workspace-wide
 * agent task snapshot (the same cache that drives per-issue activity dots);
 * WS task events invalidate it, so cards appear and disappear as agents pick
 * up and finish work with no polling.
 */
export function ActiveIssuesPage() {
  const { t } = useT("active-issues");
  const wsId = useWorkspaceId();
  const { data: snapshot = [], isLoading } = useQuery(agentTaskSnapshotOptions(wsId));

  // Unique issue ids with a running task, first-seen order preserved.
  const runningIssueIds = useMemo(() => {
    const seen = new Set<string>();
    const ids: string[] = [];
    for (const task of snapshot) {
      if (task.status !== "running" || !task.issue_id) continue;
      if (seen.has(task.issue_id)) continue;
      seen.add(task.issue_id);
      ids.push(task.issue_id);
    }
    return ids;
  }, [snapshot]);

  return (
    <div className="flex flex-1 min-h-0 flex-col">
      <PageHeader className="gap-2">
        <Activity className="h-4 w-4 text-muted-foreground" />
        <h1 className="text-sm font-medium">{t(($) => $.page.breadcrumb)}</h1>
        {runningIssueIds.length > 0 && (
          <span className="ml-1 rounded-full bg-muted px-1.5 py-0.5 text-xs text-muted-foreground">
            {runningIssueIds.length}
          </span>
        )}
      </PageHeader>

      {isLoading ? (
        <div className="grid flex-1 min-h-0 auto-rows-min grid-cols-1 gap-3 overflow-y-auto p-4 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-4">
          {Array.from({ length: 4 }).map((_, i) => (
            <Skeleton key={i} className="h-28 w-full rounded-lg" />
          ))}
        </div>
      ) : runningIssueIds.length === 0 ? (
        <div className="flex flex-1 min-h-0 flex-col items-center justify-center gap-2 text-muted-foreground">
          <Activity className="h-10 w-10 text-muted-foreground/40" />
          <p className="text-sm">{t(($) => $.page.empty_title)}</p>
          <p className="text-xs">{t(($) => $.page.empty_description)}</p>
        </div>
      ) : (
        <div className="grid flex-1 min-h-0 auto-rows-min grid-cols-1 gap-3 overflow-y-auto p-4 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-4">
          {runningIssueIds.map((id) => (
            <ActiveIssueCard key={id} issueId={id} />
          ))}
        </div>
      )}
    </div>
  );
}
