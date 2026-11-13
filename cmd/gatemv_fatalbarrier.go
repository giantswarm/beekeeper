package cmd

func (g *GateRun) fatalPublicationBarrier() error {
	return g.Host.FatalPublicationBarrier(g.Ctx, nil, g.Repo, g.Pr)
}
