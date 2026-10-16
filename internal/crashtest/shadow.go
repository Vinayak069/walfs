package crashtest

import "github.com/Vinayak069/walfs/internal/model"

func newShadow(clk *clockVar) *model.Model { return model.New(clk.now) }
