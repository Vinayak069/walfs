package crashtest

import "github.com/vinayak-singh/walfs/internal/model"

func newShadow(clk *clockVar) *model.Model { return model.New(clk.now) }
