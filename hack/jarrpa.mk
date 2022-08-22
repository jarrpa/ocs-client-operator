OCP_DIR ?= /home/jrivera/ocp/jarrpa-dev
OCP_CLUSTER_CONFIG ?= $(OCP_DIR)/install-config-aws.yaml.bak
OCP_CLUSTER_CONFIG_DIR ?= $(OCP_DIR)/aws-dev
OCP_INSTALLER ?= $(OCP_DIR)/bin/openshift-install
OCP_OC ?= $(OCP_DIR)/bin/oc
OCS_OC_PATH ?= $(OCP_OC)
KUBECTL ?= $(OCP_DIR)/bin/kubectl
KUBECONFIG ?= $(OCP_CLUSTER_CONFIG_DIR)/auth/kubeconfig
TEST_DEPLOY_DIR ?= upgrade-testing/

IMAGE_TAG ?= latest
REGISTRY_NAMESPACE ?= jarrpa
SKIP_CSV_DUMP ?= true

SUBSCRIPTION_CHANNEL = alpha

##@ Hax

TICKETGEN_DIR ?= /home/jrivera/projects/github.com/red-hat-storage/ocs-operator/hack/ticketgen
onboard-consumer: ## Create OcsClient CR
	cd $(TICKETGEN_DIR); ./ticketgen.sh key.pem > onboarding-ticket.txt
	$(OCP_OC) delete secret -n openshift-storage --ignore-not-found onboarding-ticket-key
	$(OCP_OC) create secret -n openshift-storage generic onboarding-ticket-key \
		--from-file=key=$(TICKETGEN_DIR)/pubkey.pem
	cat config/samples/odf_v1alpha1_ocsclient.yaml | $(OCP_OC) delete -n ocs-client-operator-system --ignore-not-found -f -
	export ONBOARDING_TICKET="$$(cat $(TICKETGEN_DIR)/onboarding-ticket.txt)"; echo "$${ONBOARDING_TICKET}"; \
	export PROVIDER_ENDPOINT="$$($(OCP_OC) get -n openshift-storage storagecluster -oyaml | grep ProviderEndpoint | sed "s/^.*: //")"; echo "$${PROVIDER_ENDPOINT}"; \
		cat config/samples/odf_v1alpha1_ocsclient.yaml | \
		sed "s#storageProviderEndpoint: .*#storageProviderEndpoint: \"$${PROVIDER_ENDPOINT}\"#g" | \
		sed "s#onboardingTicket: .*#onboardingTicket: \"$${ONBOARDING_TICKET}\"#g" | \
		$(OCP_OC) apply -n ocs-client-operator-system -f -


.PHONY: oc
oc: ## Run oc commands with ARGS
	${OCP_OC} ${ARGS}

watch: ## Watch it
	watch -n1 "${OCP_OC} get -n ocs-client-operator-system ocsclient,storageclassclaim,secret,cm,deployment,replicaset,po"

logs:
	$(OCP_OC) logs -n $(IMAGE_NAME)-system deployment/$(IMAGE_NAME)-controller-manager

docker-rmi: ## Remove all dangling docker images
	docker rmi --force $$(docker images -a --filter=dangling=true -q)

clear-aws-recordsets: ## AWS: clear recordsets
	aws route53 list-resource-record-sets --output json --hosted-zone-id Z087500514U36JHEM14R5 | \
	  jq '[.ResourceRecordSets[] |select(.Name|test(".*jarrpa-dev.ocs.syseng.devcluster.openshift.com."))]|map(.| { Action: "DELETE", ResourceRecordSet: .})|{Comment: "Delete jarrpa recordset",Changes: .}' | \
	  tee /tmp/recordsets.json
	aws route53 change-resource-record-sets --hosted-zone-id Z087500514U36JHEM14R5 --change-batch file:///tmp/recordsets.json || :

ocp-deploy: clear-aws-recordsets ## openshift-install create cluster
	rm -rf "${OCP_CLUSTER_CONFIG_DIR}"
	mkdir "${OCP_CLUSTER_CONFIG_DIR}"
	cp "${OCP_CLUSTER_CONFIG}" "${OCP_CLUSTER_CONFIG_DIR}/install-config.yaml"
	${OCP_INSTALLER} create cluster --dir "${OCP_CLUSTER_CONFIG_DIR}" --log-level debug

ocp-destroy: ## openshift-install destroy cluster
	${OCP_INSTALLER} destroy cluster --dir "${OCP_CLUSTER_CONFIG_DIR}" || true
	make clear-aws-recordsets

push-op: container-build container-push ## Build & Push operator image

push-bundle: bundle-build bundle-push ## Build & Push operator bundle image

push-index: catalog-build catalog-push ## Build & Push catalogcourse index image

push-all: push-op push-bundle push-index ## Build & Push all container images
