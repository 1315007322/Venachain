package vm

import (
	"encoding/json"
	"math/big"

	"github.com/PlatONEnetwork/PlatONE-Go/common"
	"github.com/PlatONEnetwork/PlatONE-Go/common/math"
	"github.com/PlatONEnetwork/PlatONE-Go/life/utils"
)

func toContractReturnValueIntType(txType int, res int64) []byte {
	if txType == common.CallContractFlag {
		return utils.Int64ToBytes(res)
	}

	bigRes := new(big.Int)
	bigRes.SetInt64(res)
	finalRes := utils.Align32Bytes(math.U256(bigRes).Bytes())
	return finalRes
}

func toContractReturnValueUintType(txType int, res uint64) []byte {
	if txType == common.CallContractFlag {
		return utils.Uint64ToBytes(res)
	}

	finalRes := utils.Align32Bytes(utils.Uint64ToBytes((res)))
	return finalRes
}

func toContractReturnValueStringType(txType int, res []byte) []byte {
	if txType == common.CallContractFlag || txType == common.TxTypeCallSollCompatibleWasm {
		return res
	}

	return MakeReturnBytes(res)
}

func toContractReturnValueStructType(txType int, res interface{}) []byte {
	b, err := json.Marshal(res)
	if err != nil {
		b = []byte{}
	}
	if txType == common.CallContractFlag || txType == common.TxTypeCallSollCompatibleWasm {
		return b
	}
	return MakeReturnBytes(b)
}

func MakeReturnBytes(ret []byte) []byte {
	var dataRealSize = len(ret)
	if (dataRealSize % 32) != 0 {
		dataRealSize = dataRealSize + (32 - (dataRealSize % 32))
	}
	dataByt := make([]byte, dataRealSize)
	copy(dataByt[0:], ret)

	strHash := common.BytesToHash(common.Int32ToBytes(32))
	sizeHash := common.BytesToHash(common.Int64ToBytes(int64(len(ret))))

	finalData := make([]byte, 0)
	finalData = append(finalData, strHash.Bytes()...)
	finalData = append(finalData, sizeHash.Bytes()...)
	finalData = append(finalData, dataByt...)

	return finalData
}
