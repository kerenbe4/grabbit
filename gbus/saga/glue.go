package saga

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"

	"github.com/opentracing/opentracing-go"
	slog "github.com/opentracing/opentracing-go/log"
	"github.com/sirupsen/logrus"
	"github.com/wework/grabbit/gbus"
	"github.com/wework/grabbit/gbus/metrics"
)

func fqnsFromMessages(objs []gbus.Message) []string {
	fqns := make([]string, 0)
	for _, obj := range objs {
		fqn := obj.SchemaName()
		fqns = append(fqns, fqn)
	}
	return fqns
}

//ErrInstanceNotFound is returned by the saga store if a saga lookup by saga id returns no valid instances
var ErrInstanceNotFound = errors.New("saga not be found")

var _ gbus.SagaGlue = &Glue{}

//Glue t/*  */ies the incoming messages from the Bus with the needed Saga instances
type Glue struct {
	*gbus.Glogged
	svcName          string
	bus              gbus.Bus
	sagaDefs         []*Def
	lock             *sync.Mutex
	alreadyRegistred map[string]bool
	msgToDefMap      map[string][]*Def
	sagaStore        Store
	timeoutManager   gbus.TimeoutManager
}

func (imsm *Glue) isSagaAlreadyRegistered(sagaType reflect.Type) bool {
	for _, def := range imsm.sagaDefs {
		if def.sagaType == sagaType {
			return true
		}
	}
	return false
}

//RegisterSaga registers the saga instance with the Bus
func (imsm *Glue) RegisterSaga(saga gbus.Saga, conf ...gbus.SagaConfFn) error {

	sagaType := reflect.TypeOf(saga)

	if imsm.isSagaAlreadyRegistered(sagaType) {
		return fmt.Errorf("saga of type %v already registered", sagaType)
	}

	imsm.sagaStore.RegisterSagaType(saga)

	def := &Def{

		glue:        imsm,
		sagaType:    sagaType,
		sagaConfFns: conf,
		startedBy:   fqnsFromMessages(saga.StartedBy()),
		msgToFunc:   make([]*MsgToFuncPair, 0),
		lock:        &sync.Mutex{}}

	saga.RegisterAllHandlers(def)
	imsm.sagaDefs = append(imsm.sagaDefs, def)
	msgNames := def.getHandledMessages()

	for _, msgName := range msgNames {
		imsm.addMsgNameToDef(msgName, def)
	}

	imsm.Log().
		WithFields(logrus.Fields{"saga_type": def.sagaType.String(), "handles_messages": len(msgNames)}).
		Info("registered saga with messages")

	return nil
}

func (imsm *Glue) addMsgNameToDef(msgName string, def *Def) {
	defs := imsm.getDefsForMsgName(msgName)
	defs = append(defs, def)
	imsm.msgToDefMap[msgName] = defs
}

func (imsm *Glue) getDefsForMsgName(msgName string) []*Def {
	defs := imsm.msgToDefMap[msgName]
	if defs == nil {
		defs = make([]*Def, 0)
	}
	return defs
}

func (imsm *Glue) handleNewSaga(def *Def, invocation gbus.Invocation, message *gbus.BusMessage) error {
	newInstance := def.newInstance()
	newInstance.StartedBy = invocation.InvokingSvc()
	newInstance.StartedBySaga = message.SagaID
	newInstance.StartedByRPCID = message.RPCID
	newInstance.StartedByMessageID = message.ID

	logInContext := invocation.Log().WithFields(logrus.Fields{"saga_def": def.String(), "saga_id": newInstance.ID})

	logInContext.
		Info("created new saga")
	if invkErr := imsm.invokeSagaInstance(def, newInstance, invocation, message); invkErr != nil {
		logInContext.Error("failed to invoke saga")
		return invkErr
	}

	if !newInstance.isComplete() {
		logInContext.Info("saving new saga")

		if e := imsm.sagaStore.SaveNewSaga(invocation.Tx(), def.sagaType, newInstance); e != nil {
			logInContext.Error("saving new saga failed")
			return e
		}

		if requestsTimeout, duration := newInstance.requestsTimeout(); requestsTimeout {
			logInContext.WithField("timeout_duration", duration).Info("new saga requested timeout")
			if tme := imsm.timeoutManager.RegisterTimeout(invocation.Tx(), newInstance.ID, duration); tme != nil {
				return tme
			}
		}
	}
	return nil
}

//SagaHandler is the generic handler invoking saga instances
func (imsm *Glue) SagaHandler(invocation gbus.Invocation, message *gbus.BusMessage) error {

	imsm.lock.Lock()
	defer imsm.lock.Unlock()
	msgName := message.PayloadFQN

	defs := imsm.msgToDefMap[strings.ToLower(msgName)]

	for _, def := range defs {
		/*
			1) If Def does not have handlers for the message type then log a warning (as this should not happen) and return
			2) Else if the message is a startup message then create new instance of a saga, invoke startup handler and mark as started
				2.1) If new instance requests timeouts then reuqest a timeout
			3) Else if message is destinated for a specific saga instance (reply messages) then find that saga by id and invoke it
			4) Else if message is not an event drop it (cmd messages should have 1 specific target)
			5) Else iterate over all instances and invoke the needed handler
		*/
		logInContext := invocation.Log().WithFields(
			logrus.Fields{"saga_def": def.String(),
				"saga_type": def.sagaType})
		startNew := def.shouldStartNewSaga(message)
		if startNew {
			return imsm.handleNewSaga(def, invocation, message)

		} else if message.SagaCorrelationID != "" {
			instance, getErr := imsm.sagaStore.GetSagaByID(invocation.Tx(), message.SagaCorrelationID)

			logInContext = logInContext.WithField("saga_correlation_id", message.SagaCorrelationID)
			if getErr != nil {
				logInContext.Error("failed to fetch saga by id")
				return getErr
			}
			if instance == nil {
				/*
					In this case we should not return an error but rather log a Warn/Info and return nil
					There are edge cases in which the instance can be nil  due to completion of the saga
					on a different node or worker.
					In cases like these returning an error here would prevent additional handlers to be invoked
					as the message will get rejected and transactions will be rolledback

					https://github.com/wework/grabbit/issues/196
				*/
				logInContext.Warn("message routed with SagaCorrelationID but no saga instance with the same id found")
				return nil
			}
			logInContext = logInContext.WithField("saga_id", instance.ID)
			def.configureSaga(instance)
			if invkErr := imsm.invokeSagaInstance(def, instance, invocation, message); invkErr != nil {
				logInContext.WithError(invkErr).Error("failed to invoke saga")
				return invkErr
			}

			return imsm.completeOrUpdateSaga(invocation.Tx(), instance)

		} else if message.Semantics == gbus.CMD {
			logInContext.Warn("command or reply message with no saga reference received")
			return errors.New("can not resolve saga instance for message")
		} else {

			logInContext.Info("fetching saga instances by type")
			instances, e := imsm.sagaStore.GetSagasByType(invocation.Tx(), def.sagaType)

			if e != nil {
				return e
			}
			logInContext.WithFields(logrus.Fields{"instances_fetched": len(instances)}).Info("fetched saga instances")

			for _, instance := range instances {
				def.configureSaga(instance)
				if invkErr := imsm.invokeSagaInstance(def, instance, invocation, message); invkErr != nil {
					logInContext.WithError(invkErr).Error("failed to invoke saga")
					return invkErr
				}
				e = imsm.completeOrUpdateSaga(invocation.Tx(), instance)
				if e != nil {
					return e
				}
			}
		}
	}

	return nil
}

func (imsm *Glue) invokeSagaInstance(def *Def, instance *Instance, invocation gbus.Invocation, message *gbus.BusMessage) error {

	span, sctx := opentracing.StartSpanFromContext(invocation.Ctx(), def.String())

	defer span.Finish()
	sginv := &sagaInvocation{
		Glogged:             &gbus.Glogged{},
		decoratedBus:        invocation.Bus(),
		decoratedInvocation: invocation,
		inboundMsg:          message,
		sagaID:              instance.ID,
		ctx:                 sctx,
		hostingSvc:          imsm.svcName,
		startedBy:           instance.StartedBy,
		startedBySaga:       instance.StartedBySaga,
		startedByMessageID:  instance.StartedByMessageID,
		startedByRPCID:      instance.StartedByRPCID,
	}
	sginv.SetLogger(invocation.Log().WithFields(logrus.Fields{
		"saga_id":  instance.ID,
		"saga_def": instance.String(),
	}))

	exchange, routingKey := invocation.Routing()
	instance.logger = imsm.Log()
	err := instance.invoke(exchange, routingKey, sginv, message)
	if err != nil {
		span.LogFields(slog.Error(err))
	}
	return err
}

func (imsm *Glue) completeOrUpdateSaga(tx *sql.Tx, instance *Instance) error {

	if instance.isComplete() {
		imsm.Log().WithField("saga_id", instance.ID).Info("saga has completed and will be deleted")

		deleteErr := imsm.sagaStore.DeleteSaga(tx, instance)
		if deleteErr != nil {
			return deleteErr
		}

		return imsm.timeoutManager.ClearTimeout(tx, instance.ID)

	}
	return imsm.sagaStore.UpdateSaga(tx, instance)
}

func (imsm *Glue) registerMessage(message gbus.Message) error {
	//only register once on each message so we will not duplicate invocations
	if _, exists := imsm.alreadyRegistred[message.SchemaName()]; exists {
		return nil
	}
	imsm.alreadyRegistred[message.SchemaName()] = true
	return imsm.bus.HandleMessage(message, imsm.SagaHandler)
}

func (imsm *Glue) registerEvent(exchange, topic string, event gbus.Message) error {

	if _, exists := imsm.alreadyRegistred[event.SchemaName()]; exists {
		return nil
	}
	imsm.alreadyRegistred[event.SchemaName()] = true
	return imsm.bus.HandleEvent(exchange, topic, event, imsm.SagaHandler)
}

//TimeoutSaga fetches a saga instance and calls its timeout interface
func (imsm *Glue) TimeoutSaga(tx *sql.Tx, sagaID string) error {

	saga, err := imsm.sagaStore.GetSagaByID(tx, sagaID)

	//we are assuming that if the TimeoutSaga has been called but no instance returned from the store the saga
	//has been completed already and
	if err == ErrInstanceNotFound {
		return nil
	}
	if err != nil {
		return err
	}

	span, _ := opentracing.StartSpanFromContext(context.Background(), "SagaTimeout")
	span.SetTag("saga_type", saga.String())
	defer span.Finish()
	timeoutErr := saga.timeout(tx, imsm.bus)

	if timeoutErr != nil {
		imsm.Log().WithError(timeoutErr).WithField("sagaID", sagaID).Error("failed to timeout saga")
		return timeoutErr
	}

	metrics.SagaTimeoutCounter.Inc()
	return imsm.completeOrUpdateSaga(tx, saga)
}

//Start starts the glue instance up
func (imsm *Glue) Start() error {
	return imsm.timeoutManager.Start()
}

//Stop starts the glue instance up
func (imsm *Glue) Stop() error {
	return imsm.timeoutManager.Stop()
}

//NewGlue creates a new Sagamanager
func NewGlue(bus gbus.Bus, sagaStore Store, svcName string, txp gbus.TxProvider, getLog func() logrus.FieldLogger, timeoutManager gbus.TimeoutManager) *Glue {
	g := &Glue{
		svcName:          svcName,
		bus:              bus,
		sagaDefs:         make([]*Def, 0),
		lock:             &sync.Mutex{},
		alreadyRegistred: make(map[string]bool),
		msgToDefMap:      make(map[string][]*Def),
		sagaStore:        sagaStore,
		timeoutManager:   timeoutManager,
	}

	logged := &gbus.Glogged{}
	g.Glogged = logged
	timeoutManager.SetTimeoutFunction(g.TimeoutSaga)
	return g
}
