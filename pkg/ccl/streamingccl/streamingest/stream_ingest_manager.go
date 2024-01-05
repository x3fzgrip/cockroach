// Copyright 2022 The Cockroach Authors.
//
// Licensed as a CockroachDB Enterprise file under the Cockroach Community
// License (the "License"); you may not use this file except in compliance with
// the License. You may obtain a copy of the License at
//
//     https://github.com/cockroachdb/cockroach/blob/master/licenses/CCL.txt

package streamingest

import (
	"context"

	"github.com/cockroachdb/cockroach/pkg/ccl/revertccl"
	"github.com/cockroachdb/cockroach/pkg/ccl/streamingccl/replicationutils"
	"github.com/cockroachdb/cockroach/pkg/ccl/utilccl"
	"github.com/cockroachdb/cockroach/pkg/jobs"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/protectedts/ptpb"
	"github.com/cockroachdb/cockroach/pkg/multitenant/mtinfopb"
	"github.com/cockroachdb/cockroach/pkg/repstream"
	"github.com/cockroachdb/cockroach/pkg/repstream/streampb"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/sql/clusterunique"
	"github.com/cockroachdb/cockroach/pkg/sql/isql"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/sql/privilege"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/eval"
	"github.com/cockroachdb/cockroach/pkg/sql/sessionprotectedts"
	"github.com/cockroachdb/cockroach/pkg/sql/syntheticprivilege"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/uuid"
	"github.com/cockroachdb/errors"
)

type streamIngestManagerImpl struct {
	evalCtx     *eval.Context
	jobRegistry *jobs.Registry
	txn         isql.Txn
	sessionID   clusterunique.ID
}

// GetReplicationStatsAndStatus implements streaming.StreamIngestManager interface.
func (r *streamIngestManagerImpl) GetReplicationStatsAndStatus(
	ctx context.Context, ingestionJobID jobspb.JobID,
) (*streampb.StreamIngestionStats, string, error) {
	return getReplicationStatsAndStatus(ctx, r.jobRegistry, r.txn, ingestionJobID)
}

// RevertTenantToTimestamp  implements streaming.StreamIngestManager interface.
func (r *streamIngestManagerImpl) RevertTenantToTimestamp(
	ctx context.Context, tenantName roachpb.TenantName, revertTo hlc.Timestamp,
) error {
	return revertTenantToTimestamp(ctx, r.evalCtx, tenantName, revertTo, r.sessionID)
}

func revertTenantToTimestamp(
	ctx context.Context, evalCtx *eval.Context, tenantName roachpb.TenantName, revertTo hlc.Timestamp, sessionID clusterunique.ID,
) error {
	execCfg := evalCtx.Planner.ExecutorConfig().(*sql.ExecutorConfig)

	// These vars are set in Txn below. This transaction checks
	// the service state of the tenant record, moves the tenant's
	// data state to ADD, and installs a PTS for the revert
	// timestamp.
	//
	// NB: We do this using a different txn since we want to be
	// able to commit the state change during the
	// non-transactional RevertSpans below.
	var (
		originalDataState mtinfopb.TenantDataState
		tenantID          roachpb.TenantID
		ptsCleanup        func()
	)
	if err := execCfg.InternalDB.Txn(ctx, func(ctx context.Context, txn isql.Txn) error {
		tenantRecord, err := sql.GetTenantRecordByName(ctx, execCfg.Settings, txn, tenantName)
		if err != nil {
			return err
		}
		tenantID, err = roachpb.MakeTenantID(tenantRecord.ID)
		if err != nil {
			return err
		}

		if tenantID.Equal(roachpb.SystemTenantID) {
			return errors.New("cannot revert the system tenant")
		}

		if tenantRecord.ServiceMode != mtinfopb.ServiceModeNone {
			return errors.Newf("cannot revert tenant %q (%d) in service mode %s; service mode must be %s",
				tenantRecord.Name,
				tenantRecord.ID,
				tenantRecord.ServiceMode,
				mtinfopb.ServiceModeNone,
			)
		}

		originalDataState = tenantRecord.DataState

		ptsCleanup, err = protectTenantSpanWithSession(ctx, txn, execCfg, tenantID, sessionID, revertTo)
		if err != nil {
			return errors.Wrap(err, "protecting revert timestamp")
		}

		// Set the data state to Add during the destructive operation.
		tenantRecord.LastRevertTenantTimestamp = revertTo
		tenantRecord.DataState = mtinfopb.DataStateAdd
		return sql.UpdateTenantRecord(ctx, execCfg.Settings, txn, tenantRecord)
	}); err != nil {
		return err
	}
	defer ptsCleanup()

	spanToRevert := keys.MakeTenantSpan(tenantID)
	if err := revertccl.RevertSpansFanout(ctx, execCfg.DB, evalCtx.JobExecContext.(sql.JobExecContext),
		[]roachpb.Span{spanToRevert},
		revertTo,
		false, /* ignoreGCThreshold */
		int64(sql.RevertTableDefaultBatchSize),
		nil /* onCompletedCallback */); err != nil {
		return err
	}

	return execCfg.InternalDB.Txn(ctx, func(ctx context.Context, txn isql.Txn) error {
		tenantRecord, err := sql.GetTenantRecordByName(ctx, execCfg.Settings, txn, tenantName)
		if err != nil {
			return err
		}
		tenantRecord.DataState = originalDataState
		return sql.UpdateTenantRecord(ctx, execCfg.Settings, txn, tenantRecord)
	})
}

func protectTenantSpanWithSession(
	ctx context.Context,
	txn isql.Txn,
	execCfg *sql.ExecutorConfig,
	tenantID roachpb.TenantID,
	sessionID clusterunique.ID,
	timestamp hlc.Timestamp,
) (func(), error) {
	ptsRecordID := uuid.MakeV4()
	ptsRecord := sessionprotectedts.MakeRecord(
		ptsRecordID,
		[]byte(sessionID.String()),
		timestamp,
		ptpb.MakeTenantsTarget([]roachpb.TenantID{tenantID}),
	)
	log.Infof(ctx, "protecting timestamp: %#+v", ptsRecord)
	pts := execCfg.ProtectedTimestampProvider.WithTxn(txn)
	if err := pts.Protect(ctx, ptsRecord); err != nil {
		return nil, err
	}
	releasePTS := func() {
		if err := execCfg.InternalDB.Txn(ctx, func(ctx context.Context, txn isql.Txn) error {
			pts := execCfg.ProtectedTimestampProvider.WithTxn(txn)
			return pts.Release(ctx, ptsRecordID)
		}); err != nil {
			log.Warningf(ctx, "failed to release protected timestamp %s: %v", ptsRecordID, err)
		}
	}
	return releasePTS, nil
}

func newStreamIngestManagerWithPrivilegesCheck(
	ctx context.Context, evalCtx *eval.Context, txn isql.Txn, sessionID clusterunique.ID,
) (eval.StreamIngestManager, error) {
	execCfg := evalCtx.Planner.ExecutorConfig().(*sql.ExecutorConfig)
	enterpriseCheckErr := utilccl.CheckEnterpriseEnabled(
		execCfg.Settings, "REPLICATION")
	if enterpriseCheckErr != nil {
		return nil, pgerror.Wrap(enterpriseCheckErr,
			pgcode.CCLValidLicenseRequired, "physical replication requires an enterprise license on the secondary (and primary) cluster")
	}

	isAdmin, err := evalCtx.SessionAccessor.HasAdminRole(ctx)
	if err != nil {
		return nil, err
	}
	if !isAdmin {
		if err := evalCtx.SessionAccessor.CheckPrivilege(ctx,
			syntheticprivilege.GlobalPrivilegeObject,
			privilege.MANAGEVIRTUALCLUSTER); err != nil {
			return nil, err
		}
	}

	return &streamIngestManagerImpl{
		evalCtx:     evalCtx,
		txn:         txn,
		jobRegistry: execCfg.JobRegistry,
		sessionID:   sessionID,
	}, nil
}

func getReplicationStatsAndStatus(
	ctx context.Context, jobRegistry *jobs.Registry, txn isql.Txn, ingestionJobID jobspb.JobID,
) (*streampb.StreamIngestionStats, string, error) {
	job, err := jobRegistry.LoadJobWithTxn(ctx, ingestionJobID, txn)
	if err != nil {
		return nil, jobspb.ReplicationError.String(), err
	}
	details, ok := job.Details().(jobspb.StreamIngestionDetails)
	if !ok {
		return nil, jobspb.ReplicationError.String(),
			errors.Newf("job with id %d is not a stream ingestion job", job.ID())
	}

	details.StreamAddress, err = redactSourceURI(details.StreamAddress)
	if err != nil {
		return nil, jobspb.ReplicationError.String(), err
	}

	stats, err := replicationutils.GetStreamIngestionStats(ctx, details, job.Progress())
	if err != nil {
		return nil, jobspb.ReplicationError.String(), err
	}
	if job.Status() == jobs.StatusPaused {
		return stats, jobspb.ReplicationPaused.String(), nil
	}
	return stats, stats.IngestionProgress.ReplicationStatus.String(), nil
}

func init() {
	repstream.GetStreamIngestManagerHook = newStreamIngestManagerWithPrivilegesCheck
}
