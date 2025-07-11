import React, { useEffect, useState } from "react";
import { Plan } from "../../gen/ts/v1/config_pb";
import { Button, Flex, Tabs, Tooltip, Typography } from "antd";
import { useAlertApi } from "../components/Alerts";
import { MAX_OPERATION_HISTORY } from "../constants";
import { backrestService } from "../api";
import {
  ClearHistoryRequestSchema,
  DoRepoTaskRequest_Task,
  DoRepoTaskRequestSchema,
  GetOperationsRequestSchema,
} from "../../gen/ts/v1/service_pb";
import { SpinButton } from "../components/SpinButton";
import { useShowModal } from "../components/ModalManager";
import { create } from "@bufbuild/protobuf";
import { useConfig } from "../components/ConfigProvider";
import { OperationListView } from "../components/OperationListView";
import { OperationTreeView } from "../components/OperationTreeView";

export const PlanView = ({ plan }: React.PropsWithChildren<{ plan: Plan }>) => {
  const [config, _] = useConfig();
  const alertsApi = useAlertApi()!;
  const showModal = useShowModal();
  const repo = config?.repos.find((r) => r.id === plan.repo);

  const handleBackupNow = async () => {
    try {
      await backrestService.backup({ value: plan.id });
      alertsApi.success("Backup scheduled.");
    } catch (e: any) {
      alertsApi.error("Failed to schedule backup: " + e.message);
    }
  };

  const handleUnlockNow = async () => {
    try {
      alertsApi.info("Unlocking repo...");
      await backrestService.doRepoTask(
        create(DoRepoTaskRequestSchema, {
          repoId: plan.repo!,
          task: DoRepoTaskRequest_Task.UNLOCK,
        })
      );
      alertsApi.success("Repo unlocked.");
    } catch (e: any) {
      alertsApi.error("Failed to unlock repo: " + e.message);
    }
  };

  const handleClearErrorHistory = async () => {
    try {
      alertsApi.info("Clearing error history...");
      await backrestService.clearHistory(
        create(ClearHistoryRequestSchema, {
          selector: {
            planId: plan.id,
            repoGuid: repo!.guid,
          },
          onlyFailed: true,
        })
      );
      alertsApi.success("Error history cleared.");
    } catch (e: any) {
      alertsApi.error("Failed to clear error history: " + e.message);
    }
  };

  if (!repo) {
    return (
      <>
        <Typography.Title>
          Repo {plan.repo} for plan {plan.id} not found
        </Typography.Title>
      </>
    );
  }

  return (
    <>
      <Flex gap="small" align="center" wrap="wrap">
        <Typography.Title>{plan.id}</Typography.Title>
      </Flex>
      <Flex gap="small" align="center" wrap="wrap">
        <SpinButton type="primary" onClickAsync={handleBackupNow}>
          Backup Now
        </SpinButton>
        <Tooltip title="Advanced users: open a restic shell to run commands on the repository. Re-index snapshots to reflect any changes in Backrest.">
          <Button
            type="default"
            onClick={async () => {
              const { RunCommandModal } = await import("./RunCommandModal");
              showModal(<RunCommandModal repo={repo} />);
            }}
          >
            Run Command
          </Button>
        </Tooltip>
        <Tooltip title="Removes lockfiles and checks the repository for errors. Only run if you are sure the repo is not being accessed by another system">
          <SpinButton type="default" onClickAsync={handleUnlockNow}>
            Unlock Repo
          </SpinButton>
        </Tooltip>
        <Tooltip title="Removes failed operations from the list">
          <SpinButton type="default" onClickAsync={handleClearErrorHistory}>
            Clear Error History
          </SpinButton>
        </Tooltip>
      </Flex>
      <Tabs
        defaultActiveKey="1"
        items={[
          {
            key: "1",
            label: "Tree View",
            children: (
              <>
                <OperationTreeView
                  req={create(GetOperationsRequestSchema, {
                    selector: {
                      instanceId: config?.instance,
                      repoGuid: repo.guid,
                      planId: plan.id!,
                    },
                    lastN: BigInt(MAX_OPERATION_HISTORY),
                  })}
                  isPlanView={true}
                />
              </>
            ),
            destroyOnHidden: true,
          },
          {
            key: "2",
            label: "List View",
            children: (
              <>
                <h2>Backup Action History</h2>
                <OperationListView
                  req={create(GetOperationsRequestSchema, {
                    selector: {
                      instanceId: config?.instance,
                      repoGuid: repo.guid,
                      planId: plan.id!,
                    },
                    lastN: BigInt(MAX_OPERATION_HISTORY),
                  })}
                  showDelete={true}
                />
              </>
            ),
            destroyOnHidden: true,
          },
        ]}
      />
    </>
  );
};
