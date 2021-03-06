// Package dsort provides distributed massively parallel resharding for very large datasets.
/*
 * Copyright (c) 2018, NVIDIA CORPORATION. All rights reserved.
 *
 */
package dsort

import (
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/fs"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Init", func() {
	BeforeEach(func() {
		ctx.smapOwner = newTestSmap("target")
		ctx.node = ctx.smapOwner.Get().Tmap["target"]
		fs.InitMountedFS()
	})

	It("should init with tar extension", func() {
		m := &Manager{ctx: dsortContext{t: cluster.NewTargetMock(nil)}}
		sr := &ParsedRequestSpec{Extension: ExtTar, Algorithm: &SortAlgorithm{Kind: SortKindNone}, MaxMemUsage: cmn.ParsedQuantity{Type: cmn.QuantityPercent, Value: 0}, DSorterType: DSorterGeneralType}
		Expect(m.init(sr)).NotTo(HaveOccurred())
		Expect(m.extractCreator.UsingCompression()).To(BeFalse())
	})

	It("should init with tgz extension", func() {
		m := &Manager{ctx: dsortContext{t: cluster.NewTargetMock(nil)}}
		sr := &ParsedRequestSpec{Extension: ExtTarTgz, Algorithm: &SortAlgorithm{Kind: SortKindNone}, MaxMemUsage: cmn.ParsedQuantity{Type: cmn.QuantityPercent, Value: 0}, DSorterType: DSorterGeneralType}
		Expect(m.init(sr)).NotTo(HaveOccurred())
		Expect(m.extractCreator.UsingCompression()).To(BeTrue())
	})

	It("should init with tar.gz extension", func() {
		m := &Manager{ctx: dsortContext{t: cluster.NewTargetMock(nil)}}
		sr := &ParsedRequestSpec{Extension: ExtTgz, Algorithm: &SortAlgorithm{Kind: SortKindNone}, MaxMemUsage: cmn.ParsedQuantity{Type: cmn.QuantityPercent, Value: 0}, DSorterType: DSorterGeneralType}
		Expect(m.init(sr)).NotTo(HaveOccurred())
		Expect(m.extractCreator.UsingCompression()).To(BeTrue())
	})

	It("should init with zip extension", func() {
		m := &Manager{ctx: dsortContext{t: cluster.NewTargetMock(nil)}}
		sr := &ParsedRequestSpec{Extension: ExtZip, Algorithm: &SortAlgorithm{Kind: SortKindNone}, MaxMemUsage: cmn.ParsedQuantity{Type: cmn.QuantityPercent, Value: 0}, DSorterType: DSorterGeneralType}
		Expect(m.init(sr)).NotTo(HaveOccurred())
		Expect(m.extractCreator.UsingCompression()).To(BeTrue())
	})
})
