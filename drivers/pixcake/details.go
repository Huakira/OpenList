package pixcake

import (
	"context"
	"net/http"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

// GetDetails 把云空间的容量信息暴露给 OpenList 的存储信息面板。
func (d *PixCake) GetDetails(ctx context.Context) (*model.StorageDetails, error) {
	var info cloudInfo
	if err := d.request(ctx, http.MethodGet, "/v1/api/cloud/info", nil, nil, &info); err != nil {
		return nil, err
	}
	total := info.TotalSpace
	used := info.UsedSpace
	if total < 0 {
		total = 0
	}
	free := total - used
	if free < 0 {
		free = 0
	}
	return &model.StorageDetails{
		DiskUsage: model.DiskUsage{
			TotalSpace: total,
			UsedSpace:  used,
		},
	}, nil
}
