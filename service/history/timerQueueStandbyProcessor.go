// Copyright (c) 2017 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package history

import (
	"fmt"
	"time"

	"github.com/uber-common/bark"
	workflow "github.com/uber/cadence/.gen/go/shared"
	"github.com/uber/cadence/common"
	"github.com/uber/cadence/common/logging"
	"github.com/uber/cadence/common/metrics"
	"github.com/uber/cadence/common/persistence"
)

type (
	timerQueueStandbyProcessorImpl struct {
		shard                   ShardContext
		historyService          *historyEngineImpl
		cache                   *historyCache
		timerTaskFilter         timerTaskFilter
		logger                  bark.Logger
		metricsClient           metrics.Client
		clusterName             string
		timerGate               RemoteTimerGate
		timerQueueProcessorBase *timerQueueProcessorBase
		timerQueueAckMgr        timerQueueAckMgr
	}
)

func newTimerQueueStandbyProcessor(shard ShardContext, historyService *historyEngineImpl, clusterName string, logger bark.Logger) *timerQueueStandbyProcessorImpl {
	timeNow := func() time.Time {
		return shard.GetCurrentTime(clusterName)
	}
	updateShardAckLevel := func(ackLevel TimerSequenceID) error {
		return shard.UpdateTimerClusterAckLevel(clusterName, ackLevel.VisibilityTimestamp)
	}
	logger = logger.WithFields(bark.Fields{
		logging.TagWorkflowCluster: clusterName,
	})
	timerTaskFilter := func(timer *persistence.TimerTaskInfo) (bool, error) {
		return verifyStandbyTask(shard, logger, clusterName, timer.DomainID, timer)
	}

	timerGate := NewRemoteTimerGate()
	timerGate.SetCurrentTime(shard.GetCurrentTime(clusterName))
	timerQueueAckMgr := newTimerQueueAckMgr(
		metrics.TimerStandbyQueueProcessorScope,
		shard,
		historyService.metricsClient,
		shard.GetTimerClusterAckLevel(clusterName),
		timeNow,
		updateShardAckLevel,
		logger,
	)
	processor := &timerQueueStandbyProcessorImpl{
		shard:           shard,
		historyService:  historyService,
		cache:           historyService.historyCache,
		timerTaskFilter: timerTaskFilter,
		logger:          logger,
		metricsClient:   historyService.metricsClient,
		clusterName:     clusterName,
		timerGate:       timerGate,
		timerQueueProcessorBase: newTimerQueueProcessorBase(
			metrics.TimerStandbyQueueProcessorScope,
			shard,
			historyService,
			timerQueueAckMgr,
			shard.GetConfig().TimerProcessorMaxPollRPS,
			shard.GetConfig().TimerProcessorStartDelay,
			logger,
		),
		timerQueueAckMgr: timerQueueAckMgr,
	}
	processor.timerQueueProcessorBase.timerProcessor = processor
	return processor
}

func (t *timerQueueStandbyProcessorImpl) Start() {
	t.timerQueueProcessorBase.Start()
}

func (t *timerQueueStandbyProcessorImpl) Stop() {
	t.timerQueueProcessorBase.Stop()
}

func (t *timerQueueStandbyProcessorImpl) getTimerFiredCount() uint64 {
	return t.timerQueueProcessorBase.getTimerFiredCount()
}

func (t *timerQueueStandbyProcessorImpl) getTimerGate() TimerGate {
	return t.timerGate
}

func (t *timerQueueStandbyProcessorImpl) setCurrentTime(currentTime time.Time) {
	t.timerGate.SetCurrentTime(currentTime)
}

func (t *timerQueueStandbyProcessorImpl) retryTasks() {
	t.timerQueueProcessorBase.retryTasks()
}

// NotifyNewTimers - Notify the processor about the new standby timer events arrival.
// This should be called each time new timer events arrives, otherwise timers maybe fired unexpected.
func (t *timerQueueStandbyProcessorImpl) notifyNewTimers(timerTasks []persistence.Task) {
	t.timerQueueProcessorBase.notifyNewTimers(timerTasks)
}

func (t *timerQueueStandbyProcessorImpl) process(timerTask *persistence.TimerTaskInfo) (int, error) {
	ok, err := t.timerTaskFilter(timerTask)
	if err != nil {
		return metrics.TimerStandbyQueueProcessorScope, err
	} else if !ok {
		t.timerQueueAckMgr.completeTimerTask(timerTask)
		return metrics.TimerStandbyQueueProcessorScope, nil
	}

	taskID := TimerSequenceID{VisibilityTimestamp: timerTask.VisibilityTimestamp, TaskID: timerTask.TaskID}
	t.logger.Debugf("Processing timer: (%s), for WorkflowID: %v, RunID: %v, Type: %v, TimeoutType: %v, EventID: %v",
		taskID, timerTask.WorkflowID, timerTask.RunID, t.timerQueueProcessorBase.getTimerTaskType(timerTask.TaskType),
		workflow.TimeoutType(timerTask.TimeoutType).String(), timerTask.EventID)

	switch timerTask.TaskType {
	case persistence.TaskTypeUserTimer:
		return metrics.TimerStandbyTaskUserTimerScope, t.processExpiredUserTimer(timerTask)

	case persistence.TaskTypeActivityTimeout:
		return metrics.TimerStandbyTaskActivityTimeoutScope, t.processActivityTimeout(timerTask)

	case persistence.TaskTypeDecisionTimeout:
		return metrics.TimerStandbyTaskDecisionTimeoutScope, t.processDecisionTimeout(timerTask)

	case persistence.TaskTypeWorkflowTimeout:
		return metrics.TimerStandbyTaskWorkflowTimeoutScope, t.processWorkflowTimeout(timerTask)

	case persistence.TaskTypeActivityRetryTimer:
		return metrics.TimerStandbyTaskActivityRetryTimerScope, nil // retry backoff timer should not get created on passive cluster

	case persistence.TaskTypeWorkflowRetryTimer:
		return metrics.TimerStandbyTaskWorkflowRetryTimerScope, t.processWorkflowRetryTimerTask(timerTask)

	case persistence.TaskTypeDeleteHistoryEvent:
		return metrics.TimerStandbyTaskDeleteHistoryEvent, t.timerQueueProcessorBase.processDeleteHistoryEvent(timerTask)

	default:
		return metrics.TimerStandbyQueueProcessorScope, errUnknownTimerTask
	}
}

func (t *timerQueueStandbyProcessorImpl) processExpiredUserTimer(timerTask *persistence.TimerTaskInfo) error {

	return t.processTimer(timerTask, func(msBuilder mutableState) error {
		executionInfo := msBuilder.GetExecutionInfo()
		tBuilder := t.historyService.getTimerBuilder(&workflow.WorkflowExecution{
			WorkflowId: common.StringPtr(executionInfo.WorkflowID),
			RunId:      common.StringPtr(executionInfo.RunID),
		})

	ExpireUserTimers:
		for _, td := range tBuilder.GetUserTimers(msBuilder) {
			hasTimer, _ := tBuilder.GetUserTimer(td.TimerID)
			if !hasTimer {
				t.logger.Debugf("Failed to find in memory user timer: %s", td.TimerID)
				return fmt.Errorf("Failed to find in memory user timer: %s", td.TimerID)
			}
			if !td.TaskCreated {
				break ExpireUserTimers
			}

			if isExpired := tBuilder.IsTimerExpired(td, timerTask.VisibilityTimestamp); isExpired {
				// active cluster will add an timer fired event and schedule a decision if necessary
				// standby cluster should just call ack manager to retry this task
				// since we are stilling waiting for the fired event to be replicated
				//
				// we do not need to notity new timer to base, since if there is no new event being replicated
				// checking again if the timer can be completed is meaningless
				return ErrTaskRetry
			}
			// since the user timer are already sorted, so if there is one timer which will not expired
			// all user timer after this timer will not expired
			break ExpireUserTimers
		}
		// if there is no user timer expired, then we are good
		return nil
	})
}

func (t *timerQueueStandbyProcessorImpl) processActivityTimeout(timerTask *persistence.TimerTaskInfo) error {

	return t.processTimer(timerTask, func(msBuilder mutableState) error {
		executionInfo := msBuilder.GetExecutionInfo()
		tBuilder := t.historyService.getTimerBuilder(&workflow.WorkflowExecution{
			WorkflowId: common.StringPtr(executionInfo.WorkflowID),
			RunId:      common.StringPtr(executionInfo.RunID),
		})

	ExpireActivityTimers:
		for _, td := range tBuilder.GetActivityTimers(msBuilder) {
			_, isRunning := msBuilder.GetActivityInfo(td.ActivityID)
			if !isRunning {
				//  We might have time out this activity already.
				continue ExpireActivityTimers
			}
			if !td.TaskCreated {
				break ExpireActivityTimers
			}

			if isExpired := tBuilder.IsTimerExpired(td, timerTask.VisibilityTimestamp); isExpired {
				// active cluster will add an activity timeout event and schedule a decision if necessary
				// standby cluster should just call ack manager to retry this task
				// since we are stilling waiting for the activity timeout event to be replicated
				//
				// we do not need to notity new timer to base, since if there is no new event being replicated
				// checking again if the timer can be completed is meaningless
				return ErrTaskRetry
			}
			// since the activity timer are already sorted, so if there is one timer which will not expired
			// all activity timer after this timer will not expired
			break ExpireActivityTimers
		}
		// if there is no user timer expired, then we are good
		return nil
	})
}

func (t *timerQueueStandbyProcessorImpl) processDecisionTimeout(timerTask *persistence.TimerTaskInfo) error {

	return t.processTimer(timerTask, func(msBuilder mutableState) error {
		di, isPending := msBuilder.GetPendingDecision(timerTask.EventID)

		if !isPending {
			return nil
		}

		ok, err := verifyTaskVersion(t.shard, t.logger, timerTask.DomainID, di.Version, timerTask.Version, timerTask)
		if err != nil {
			return err
		} else if !ok {
			return nil
		}

		// active cluster will add an decision timeout event and schedule a decision
		// standby cluster should just call ack manager to retry this task
		// since we are stilling waiting for the decision timeout event / decision completion to be replicated
		//
		// we do not need to notity new timer to base, since if there is no new event being replicated
		// checking again if the timer can be completed is meaningless
		return ErrTaskRetry
	})
}

func (t *timerQueueStandbyProcessorImpl) processWorkflowRetryTimerTask(timerTask *persistence.TimerTaskInfo) error {

	return t.processTimer(timerTask, func(msBuilder mutableState) error {

		nextEventID := msBuilder.GetNextEventID()

		if nextEventID > common.FirstEventID+1 {
			// first decision already scheduled
			return nil
		}

		ok, err := verifyTaskVersion(t.shard, t.logger, timerTask.DomainID, msBuilder.GetExecutionInfo().DecisionVersion, timerTask.Version, timerTask)
		if err != nil {
			return err
		} else if !ok {
			return nil
		}

		// active cluster will add first decision task after backoff timeout.
		// standby cluster should just call ack manager to retry this task
		// since we are stilling waiting for the first DecisionSchedueldEvent to be replicated from active side.
		//
		// we do not need to notity new timer to base, since if there is no new event being replicated
		// checking again if the timer can be completed is meaningless
		return ErrTaskRetry
	})
}

func (t *timerQueueStandbyProcessorImpl) processWorkflowTimeout(timerTask *persistence.TimerTaskInfo) error {

	return t.processTimer(timerTask, func(msBuilder mutableState) error {
		// we do not need to notity new timer to base, since if there is no new event being replicated
		// checking again if the timer can be completed is meaningless

		ok, err := verifyTaskVersion(t.shard, t.logger, timerTask.DomainID, msBuilder.GetStartVersion(), timerTask.Version, timerTask)
		if err != nil {
			return err
		} else if !ok {
			return nil
		}

		return ErrTaskRetry
	})
}

func (t *timerQueueStandbyProcessorImpl) processTimer(timerTask *persistence.TimerTaskInfo, fn func(mutableState) error) (retError error) {
	context, release, err := t.cache.getOrCreateWorkflowExecution(t.timerQueueProcessorBase.getDomainIDAndWorkflowExecution(timerTask))
	if err != nil {
		return err
	}
	defer func() {
		if retError == ErrTaskRetry {
			release(nil)
		} else {
			release(retError)
		}
	}()

	msBuilder, err := loadMutableStateForTimerTask(context, timerTask, t.metricsClient, t.logger)
	if err != nil {
		return err
	} else if msBuilder == nil {
		return nil
	}

	if !msBuilder.IsWorkflowExecutionRunning() {
		// workflow already finished, no need to process the timer
		return nil
	}

	return fn(msBuilder)

}
