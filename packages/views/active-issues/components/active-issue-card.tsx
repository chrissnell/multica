"use client";

import { useQuery } from "@tanstack/react-query";
import { Skeleton } from "@multica/ui/components/ui/skeleton";
import { useWorkspaceId } from "@multica/core/hooks";
import { useWorkspacePaths } from "@multica/core/paths";
import { issueDetailOptions } from "@multica/core/issues/queries";
import { AppLink } from "../../navigation";
import { ActorAvatar } from "../../common/actor-avatar";
import { StatusIcon } from "../../issues/components/status-icon";
import { PriorityIcon } from "../../issues/components/priority-icon";
import { IssueAgentActivityIndicator } from "../../issues/components/issue-agent-activity-indicator";

/**
 * Read-only card for one issue with a running agent. The issue id comes from
 * the agent task snapshot, which carries no issue body — so each card resolves
 * its own issue via the shared detail cache (same pattern as the sidebar's
 * PinRow). The whole card is the navigation target for the issue page.
 *
 * A 404 (issue deleted out from under a still-running task) renders nothing
 * rather than a broken row; the snapshot catches up on the next WS event.
 */
export function ActiveIssueCard({ issueId }: { issueId: string }) {
  const wsId = useWorkspaceId();
  const p = useWorkspacePaths();
  const { data: issue, isPending, isError } = useQuery(issueDetailOptions(wsId, issueId));

  if (isPending) return <Skeleton className="h-28 w-full rounded-lg" />;
  if (isError || !issue) return null;

  const hasAssignee = !!issue.assignee_type && !!issue.assignee_id;

  return (
    <AppLink href={p.issueDetail(issue.id)} className="group/card block">
      <div className="rounded-lg border-[0.5px] border-border bg-card py-3 px-2.5 shadow-[0_3px_6px_-2px_rgba(0,0,0,0.02),0_1px_1px_0_rgba(0,0,0,0.04)] transition-colors group-hover/card:border-accent group-hover/card:bg-accent">
        {/* Row 1: status + priority + identifier (left), agent activity (right) */}
        <div className="flex items-center justify-between gap-2">
          <div className="flex min-w-0 items-center gap-1.5">
            <StatusIcon status={issue.status} className="!size-3.5 shrink-0" />
            <PriorityIcon priority={issue.priority} />
            <p className="truncate text-xs text-muted-foreground">{issue.identifier}</p>
          </div>
          <IssueAgentActivityIndicator issueId={issue.id} />
        </div>

        {/* Row 2: title */}
        <p className="mt-1 text-sm font-medium leading-snug line-clamp-2">{issue.title}</p>

        {/* Row 3: assignee */}
        {hasAssignee && (
          <div className="mt-2 flex items-center justify-end">
            <ActorAvatar
              actorType={issue.assignee_type!}
              actorId={issue.assignee_id!}
              size={20}
              enableHoverCard
              className="shrink-0"
            />
          </div>
        )}
      </div>
    </AppLink>
  );
}
