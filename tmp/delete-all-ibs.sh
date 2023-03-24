#!/bin/bash

for ib in $(kubectl get ib -o=name)
do
    print "deleting ib ${ib}"
    kubectl delete ib ${ib}
done

