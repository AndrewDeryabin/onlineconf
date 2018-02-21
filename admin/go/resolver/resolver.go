package resolver

import (
	"context"
	"github.com/rs/zerolog/log"
	"time"
)

var treeI tree

func init() {
	var ctx = context.Background()
	ctx = log.Logger.WithContext(ctx)
	if err := treeI.update(ctx); err != nil {
		log.Ctx(ctx).Fatal().Err(err).Msg("failed to initialize tree")
	}
	go func() {
		c := time.Tick(5 * time.Second)
		for _ = range c {
			if err := treeI.update(ctx); err != nil {
				log.Ctx(ctx).Error().Err(err).Msg("failed to update tree")
			}
		}
	}()
}
