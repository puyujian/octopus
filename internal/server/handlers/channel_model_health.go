package handlers

import (
	"net/http"

	"github.com/bestruirui/octopus/internal/channelmodel"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/server/middleware"
	"github.com/bestruirui/octopus/internal/server/resp"
	"github.com/bestruirui/octopus/internal/server/router"
	"github.com/gin-gonic/gin"
)

func init() {
	router.NewGroupRouter("/api/v1/channel-model").
		Use(middleware.Auth()).
		Use(middleware.RequireJSON()).
		AddRoute(router.NewRoute("/health/query", http.MethodPost).Handle(queryChannelModelHealth)).
		AddRoute(router.NewRoute("/health/run", http.MethodPost).Handle(runChannelModelHealth)).
		AddRoute(router.NewRoute("/group/preview", http.MethodPost).Handle(previewChannelModelGroups)).
		AddRoute(router.NewRoute("/group/apply", http.MethodPost).Handle(applyChannelModelGroups))
}

func queryChannelModelHealth(c *gin.Context) {
	var req model.ChannelModelHealthRunRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.InvalidJSON(c)
		return
	}
	rows, err := channelmodel.Query(c.Request.Context(), req.Targets)
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, rows)
}

func runChannelModelHealth(c *gin.Context) {
	var req model.ChannelModelHealthRunRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.InvalidJSON(c)
		return
	}
	taskID, count, err := channelmodel.Run(c.Request.Context(), req.Targets)
	if err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	c.JSON(http.StatusAccepted, resp.ResponseStruct{
		Code:    http.StatusAccepted,
		Message: "accepted",
		Data: model.ChannelModelHealthRunAccepted{
			TaskID: taskID,
			Count:  count,
		},
	})
}

func previewChannelModelGroups(c *gin.Context) {
	var req model.ChannelModelGroupPreviewRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.InvalidJSON(c)
		return
	}
	items, err := channelmodel.Preview(c.Request.Context(), req.Targets)
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, items)
}

func applyChannelModelGroups(c *gin.Context) {
	var req model.ChannelModelGroupApplyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.InvalidJSON(c)
		return
	}
	result, err := channelmodel.Apply(c.Request.Context(), req)
	if err != nil {
		resp.ErrorWithAppError(c, http.StatusInternalServerError, err)
		return
	}
	resp.Success(c, result)
}
