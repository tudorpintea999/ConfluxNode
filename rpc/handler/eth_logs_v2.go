package handler

import (
	"context"
	"errors"

	"github.com/conflux-chain/conflux-infura/rpc/ethbridge"
	"github.com/conflux-chain/conflux-infura/store"
	"github.com/conflux-chain/conflux-infura/store/mysql"
	"github.com/openweb3/web3go/client"
	"github.com/openweb3/web3go/types"
)

type EthLogsApiHandlerV2 struct {
	ms *mysql.MysqlStore

	networkId uint32
}

func (handler *EthLogsApiHandlerV2) GetLogs(
	ctx context.Context,
	eth *client.RpcEthClient,
	filter *types.FilterQuery,
) ([]types.Log, bool, error) {
	timeoutCtx, cancel := context.WithTimeout(ctx, store.TimeoutGetLogs)
	defer cancel()

	// record the reorg version before query to ensure data consistence
	lastReorgVersion, err := handler.ms.GetReorgVersion()
	if err != nil {
		return nil, false, err
	}

	for {
		logs, hitStore, err := handler.getLogsReorgGuard(timeoutCtx, eth, filter)
		if err != nil {
			return nil, false, err
		}

		// check the reorg version after query
		reorgVersion, err := handler.ms.GetReorgVersion()
		if err != nil {
			return nil, false, err
		}

		if reorgVersion == lastReorgVersion {
			return logs, hitStore, nil
		}

		// when reorg occurred, check timeout before retry.
		select {
		case <-timeoutCtx.Done():
			return nil, false, store.ErrGetLogsTimeout
		default:
		}

		// reorg version changed during data query and try again.
		lastReorgVersion = reorgVersion
	}
}

func (handler *EthLogsApiHandlerV2) getLogsReorgGuard(
	ctx context.Context,
	eth *client.RpcEthClient,
	filter *types.FilterQuery,
) ([]types.Log, bool, error) {
	// Try to query event logs from database and fullnode.
	dbFilter, fnFilter, err := handler.splitLogFilter(eth, filter)
	if err != nil {
		return nil, false, err
	}

	var logs []types.Log

	// query data from database
	if dbFilter != nil {
		dbLogs, err := handler.ms.GetLogsV2(ctx, *dbFilter)
		if err != nil {
			// TODO ErrPrunedAlready
			return nil, false, err
		}

		for _, v := range dbLogs {
			cfxLog, ext := v.ToCfxLog()
			logs = append(logs, *ethbridge.ConvertLog(cfxLog, ext))
		}
	}

	// query data from fullnode
	if fnFilter != nil {
		fnLogs, err := eth.Logs(*fnFilter)
		if err != nil {
			return nil, false, err
		}

		logs = append(logs, fnLogs...)
	}

	return logs, dbFilter != nil, nil
}

func (handler *EthLogsApiHandlerV2) splitLogFilter(
	eth *client.RpcEthClient,
	filter *types.FilterQuery,
) (*store.LogFilterV2, *types.FilterQuery, error) {
	maxBlock, ok, err := handler.ms.MaxEpoch()
	if err != nil {
		return nil, nil, err
	}

	if !ok {
		return nil, filter, nil
	}

	if filter.BlockHash != nil {
		return handler.splitLogFilterByBlockHash(eth, filter, maxBlock)
	}

	return handler.splitLogFilterByBlockRange(eth, filter, maxBlock)
}

func (handler *EthLogsApiHandlerV2) splitLogFilterByBlockHash(
	eth *client.RpcEthClient,
	filter *types.FilterQuery,
	maxBlock uint64,
) (*store.LogFilterV2, *types.FilterQuery, error) {
	block, err := eth.BlockByHash(*filter.BlockHash, false)
	if err != nil {
		return nil, nil, err
	}

	if block == nil {
		return nil, nil, errors.New("unknown block")
	}

	bn := block.Number.Uint64()

	if bn > maxBlock {
		return nil, filter, nil
	}

	dbFilter := store.ParseEthLogFilterV2(bn, bn, filter, handler.networkId)
	return &dbFilter, nil, err
}

func (handler *EthLogsApiHandlerV2) splitLogFilterByBlockRange(
	eth *client.RpcEthClient,
	filter *types.FilterQuery,
	maxBlock uint64,
) (*store.LogFilterV2, *types.FilterQuery, error) {
	if filter.FromBlock == nil || *filter.FromBlock < 0 {
		return nil, filter, nil
	}

	if filter.ToBlock == nil || *filter.ToBlock < 0 {
		return nil, filter, nil
	}

	blockFrom, blockTo := uint64(*filter.FromBlock), uint64(*filter.ToBlock)

	// no data in database
	if blockFrom > maxBlock {
		return nil, filter, nil
	}

	// all data in database
	if blockTo <= maxBlock {
		dbFilter := store.ParseEthLogFilterV2(blockFrom, blockTo, filter, handler.networkId)
		return &dbFilter, nil, nil
	}

	// otherwise, partial data in databse
	dbFilter := store.ParseEthLogFilterV2(blockFrom, maxBlock, filter, handler.networkId)
	fnBlockFrom := types.BlockNumber(maxBlock + 1)
	fnFilter := types.FilterQuery{
		FromBlock: &fnBlockFrom,
		ToBlock:   filter.ToBlock,
		Addresses: filter.Addresses,
		Topics:    filter.Topics,
	}

	return &dbFilter, &fnFilter, nil
}
