// Package reb provides resilvering and rebalancing functionality for the AIStore object storage.
/*
 * Copyright (c) 2019, NVIDIA CORPORATION. All rights reserved.
 */
package reb

import (
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/memsys"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

type testObject struct {
	bck  cmn.Bck
	name string
}

var _ = Describe("ECWaiter", func() {
	It("Checking EC slice waiter", func() {
		const (
			sliceCnt     = 3
			sliceDone    = 2
			toCleanLastN = 2
		)
		wt := newWaiter(memsys.DefaultPageMM())
		// must have more than ecRebBatchSize items
		objs := []testObject{
			{bck: cmn.Bck{Name: "bck1", Provider: cmn.ProviderAmazon, Ns: cmn.NsGlobal}, name: "obj1"},
			{bck: cmn.Bck{Name: "bck1", Provider: cmn.ProviderAmazon, Ns: cmn.NsGlobal}, name: "obj2"},
			{bck: cmn.Bck{Name: "bck1", Provider: cmn.ProviderAIS, Ns: cmn.NsGlobal}, name: "obj2"},
			{bck: cmn.Bck{Name: "bck1", Provider: cmn.ProviderAIS, Ns: cmn.NsGlobal}, name: "obj3"},
			{bck: cmn.Bck{Name: "bck2", Provider: cmn.ProviderAmazon, Ns: cmn.NsGlobal}, name: "obj1"},
			{bck: cmn.Bck{Name: "bck2", Provider: cmn.ProviderAIS, Ns: cmn.NsGlobal}, name: "obj4"},
			{bck: cmn.Bck{Name: "bck5", Provider: cmn.ProviderAmazon, Ns: cmn.NsGlobal}, name: "obj1"},
			{bck: cmn.Bck{Name: "bck5", Provider: cmn.ProviderAmazon, Ns: cmn.NsGlobal}, name: "obj2"},
			{bck: cmn.Bck{Name: "bck5", Provider: cmn.ProviderAIS, Ns: cmn.NsGlobal}, name: "obj3"},
			{bck: cmn.Bck{Name: "bck5", Provider: cmn.ProviderAIS, Ns: cmn.NsGlobal}, name: "obj4"},
			{bck: cmn.Bck{Name: "bck5", Provider: cmn.ProviderAmazon, Ns: cmn.NsGlobal}, name: "obj5"},
			{bck: cmn.Bck{Name: "bck5", Provider: cmn.ProviderAmazon, Ns: cmn.NsGlobal}, name: "obj6"},
			{bck: cmn.Bck{Name: "bck5", Provider: cmn.ProviderAIS, Ns: cmn.NsGlobal}, name: "obj8"},
			{bck: cmn.Bck{Name: "bck5", Provider: cmn.ProviderAIS, Ns: cmn.NsGlobal}, name: "obj9"},
		}
		rebObjs := make([]*rebObject, 0)
		created := make([]*waitCT, 0, len(rebObjs))

		By("all unames must be unique")
		objSet := make(map[string]struct{})
		for _, o := range objs {
			uname := uniqueWaitID(o.bck, o.name)
			objSet[uname] = struct{}{}
			rebObjs = append(rebObjs, &rebObject{uid: uname})
		}
		// Here and below it should be Expect(objSet).To(HaveLen(N)) but it
		// makes ginkgo to print the entire list on error that generates
		// megabytes of zeros in my case - because it prints even SGL's put
		// and get byte slices
		Expect(len(objSet)).To(Equal(len(rebObjs)), "Some objects did not get unique ID")

		By("created unique waiter for each slice")
		for _, o := range rebObjs {
			for i := 0; i < sliceCnt; i++ {
				ws := wt.lookupCreate(o.uid, int16(i), waitForSingleSlice)
				// Make sure this pointer is unique
				for _, s := range created {
					// NotTo(Equal) does not work since it must compare pointers
					Expect(s).NotTo(BeIdenticalTo(ws), o.uid)
				}
				created = append(created, ws)
			}
		}
		Expect(len(created)).To(Equal(len(rebObjs)*sliceCnt), "Some slices did not get unique ID")

		By("adding the same slices should return existing waiters")
		for _, o := range rebObjs {
			for i := 0; i < sliceCnt; i++ {
				ws := wt.lookupCreate(o.uid, int16(i), waitForSingleSlice)
				found := false
				// make sure that it returns a pointer created at previoius step
				for _, s := range created {
					if s == ws {
						found = true
						break
					}
				}
				Expect(found).Should(BeTrue(), o.uid)
			}
		}
		// `int` conversion required because `waitSliceCnt` returns a value
		// of an atomic variable, and atomic variable cannot be just `int`
		Expect(wt.waitFor.Load()).To(BeEquivalentTo(len(rebObjs) * sliceCnt))

		By("marking a few waiters done must decrease waiter counter")
		for i := 0; i < sliceDone; i++ {
			wt.waitFor.Dec()
		}

		By("cleaning up invalid batch should not change waiter list")
		wt.cleanupBatch(rebObjs, len(rebObjs)+10)
		Expect(len(wt.objs)).To(Equal(len(rebObjs)))

		By("cleanup in the middle")
		Expect(len(objs)).Should(BeNumerically(">", ecRebBatchSize+4))
		wt.cleanupBatch(rebObjs, len(rebObjs)-ecRebBatchSize-3)
		Expect(len(wt.objs)).To(Equal(len(rebObjs) - ecRebBatchSize))
		// after that first and last item should still exist, so "creating"
		// waitSlice for them once more should return existing ones and
		// must not change the size of waitSlice length
		currLen := len(wt.objs)
		_ = wt.lookupCreate(rebObjs[0].uid, 1, waitForSingleSlice)
		_ = wt.lookupCreate(rebObjs[len(rebObjs)-1].uid, 1, waitForSingleSlice)
		Expect(len(wt.objs)).To(Equal(currLen))

		By("cleanup last incomplete batch (a few last items)")
		wt.cleanupBatch(rebObjs, len(rebObjs)-toCleanLastN)
		Expect(len(wt.objs)).To(Equal(len(rebObjs) - ecRebBatchSize - toCleanLastN))

		By("cleanup everything")
		wt.cleanup()
		Expect(wt.waitFor.Load()).To(BeZero())
		Expect(len(wt.objs)).To(BeZero())
	})
})
