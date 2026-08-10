package agentgateway

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// catalogSchemaDigest stores the schema identities generated from the current
// Agent capability descriptors.  The table intentionally stores only SHA-256
// values, never schema bodies or credential-bearing fields.
//
// Source of truth:
//   - dirextalk-agent/internal/agentcapability/misc_capabilities.go
//   - dirextalk-agent/internal/agentcapability/web_search_capability.go
//   - dirextalk-agent/internal/agentcapability/core_adapters.go
//   - dirextalk-agent/internal/agentcapability/core_schedule_adapter.go
//   - dirextalk-agent/internal/agentcapability/aws_adapter.go
//   - dirextalk-agent/internal/agentcapability/voice_adapter.go
//   - dirextalk-agent/internal/agentcapability/executionv2/capability.go
//
// Keep this table in lockstep with the Agent descriptor constructors. A
// catalog drift test exercises the table through ValidateCatalog so changing
// an advertised schema without updating this proof cannot pass readiness.
type catalogSchemaDigest struct {
	inputHex  string
	resultHex string
	eventHex  string
}

var expectedCatalogSchemaDigests = map[string]catalogSchemaDigest{
	"agent.account.deprovision": {
		inputHex:  "3874b70656e828ee63dce067feaca287f858875e1354b21a83e7af8be915df8a",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.config.get": {
		inputHex:  "99334726611ccf58a148b0814696bfa6fe08c1b2d027e946beccf5a74331c9aa",
		resultHex: "b405eb007985d32e32e87c8eadb3599b13123dd790939e00a80b41da03e1f0d9",
	},
	"agent.config.update": {
		inputHex:  "44da7f228706998fd7e631c8d7e3aaed945d6fe794dd7cfd295c6c02e9f27368",
		resultHex: "b405eb007985d32e32e87c8eadb3599b13123dd790939e00a80b41da03e1f0d9",
	},
	"agent.config.propose_patch": {
		inputHex:  "dde1e726d312471001f873c69302d2fe452258d808bf5abb0f6d7b9c8de92907",
		resultHex: "b547df800957f22d72b72c261ae70bb0a038542f6345940b5f2ec9122fcb000d",
	},
	"agent.backends.get": {
		inputHex:  "99334726611ccf58a148b0814696bfa6fe08c1b2d027e946beccf5a74331c9aa",
		resultHex: "4a0f95cd99ddf917e51efbf74e83f2dd78775f7602437f9afe31df0a25e82d19",
	},
	"agent.core.status.get": {
		inputHex:  "99334726611ccf58a148b0814696bfa6fe08c1b2d027e946beccf5a74331c9aa",
		resultHex: "677f2cc7224b592e8bebc7e63eeecef4a63bda3b74fbe673999fc5bf559b675e",
	},
	"agent.chat": {
		inputHex:  "5a7ef7ea09ccf4dcdb6a8f120c74b18d7ef9b2e7912d203947c2238e2476afe1",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.chat.stream": {
		inputHex:  "2ccc801e7bb77acc1fff9ec217651b73408b1ce5ca0c39fd620f1b98c85a0fcf",
		resultHex: "e517caf92e89459a4b9e6318b519765499bfa0e30c077c0bf004cfd852ea5545",
		eventHex:  "dad787ab7255e30302327d0cc1467503d43ed5f1ec9ff869d968edb810e98966",
	},
	"agent.chat.attachment.begin": {
		inputHex:  "8ca4c5efd311f5793aaabbab7813c8e8588f844b8cc1a0b9d0ec3a396be06e47",
		resultHex: "72fe45efd99e1170083007800597665df3a8a8db8e78795ac585a42ff2fb95e8",
	},
	"agent.chat.attachment.append": {
		inputHex:  "f3c65322ece7aca25c93e12046b4919aa6b942e5e252b3802ba86184898e013e",
		resultHex: "72fe45efd99e1170083007800597665df3a8a8db8e78795ac585a42ff2fb95e8",
	},
	"agent.chat.attachment.commit": {
		inputHex:  "1f5a339393a28aaa60a08d4ba3d8e43d4320eef387d59f3524a4fb598320c875",
		resultHex: "394f7f4be76a9433531de881cf34aa4359342f9420ab7cd3308e50bc6470d01d",
	},
	"agent.chat.conversations.create": {
		inputHex:  "f4882cae6623ba2a5f487ae5004eff65d55f9fefb41d69b8e03e9e4e2275754f",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.chat.conversations.list": {
		inputHex:  "4be38b2951e4fd2b2c47a6cda142ec2dd648478a29ed560c263431c3d2ca5a1a",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.chat.conversations.get": {
		inputHex:  "a6f5998471ffc56ef36ae609aca240371a06696f5712fb63c928f77c184d9599",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.chat.conversations.rename": {
		inputHex:  "15ce8e631d7362c6abf22dde0deaac4f9babefb18f2db497dcbaa7f6f70ce52f",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.chat.conversations.delete": {
		inputHex:  "b6d2f782e3af256e74d81ddc89fda05f775933272633a5aca6e998167dd027cc",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.chat.turns.list": {
		inputHex:  "30681a058e34eaaf4cbbfe914762501b9dc2c8c8891a297e0ea458369fe6cf75",
		resultHex: "c6bd313ead8cf85cf1855e16018701eb5bf2508f1abfd495ec77ea50a45f9903",
	},
	"agent.chat.turn.stop": {
		inputHex:  "d7bc619c13ed4ab5b743b7157d80e1a303386d1259696f19b5d82cfb939e1058",
		resultHex: "5031fafc12966ca78f1c41730d87f967f622647042719a67dca2619cfb737763",
	},
	"agent.chat.turn.steer": {
		inputHex:  "45266f9289b16504974bc0e318c66dc37ef89c9c0fce21936f2b805f58ea1307",
		resultHex: "f435121e93acac358d4dc1e4725d3be97763c0f5dfeca03ac4d315663029331f",
	},
	"agent.context.compress": {
		inputHex:  "e482c09e35d5c78ff7205b9ca1cb91509ed9432ec707de9ed25143f635869523",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.summarize": {
		inputHex:  "3b9877b3e468172efe92acd1d4b8d135d1b817e88149e11bb2092b785b9e230a",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.models.list": {
		inputHex:  "7bad0da3198a85f725bc0090f117070fb66d8da6ce787d12956e5d4cd9bc4ffe",
		resultHex: "52078e2bf86ed500efb85a81acfcfebe601b16d4ad169760c693320cc6fe7fca",
	},
	"agent.runtime.inspect": {
		inputHex:  "99334726611ccf58a148b0814696bfa6fe08c1b2d027e946beccf5a74331c9aa",
		resultHex: "914d124d30c28d0e9b236cb778db53eb40825d00e4db318384cdd2e38e2f05f8",
	},
	"agent.runtime.install": {
		inputHex:  "53c95bff58d4b58ef32f35d248ff0838d1b033f19d16981aa6872d05185973af",
		resultHex: "7f6422a392affeafa1933f0950be9ea8c9e2660adfa60f0ddcbfdbb408a9d5d9",
	},
	"agent.runtime.which": {
		inputHex:  "fc53b8849007c594f332df3a348ad1d52ebdf922f26b7259eb35724a210b4ea2",
		resultHex: "4cd8e91ae4594efa9d2bf783efd772f45679d691fce4a61369f825bd18ac7703",
	},
	"agent.runtime.run": {
		inputHex:  "0b388d63b462f0a4eba76f646cee3928db2319a7753549c6ee0fc3536f43ba81",
		resultHex: "81e79bdd93a989e5379b33aca2aa833a2dd9af4d2d607c9901c264f43bf915fb",
	},
	"agent.web_search.config.get": {
		inputHex:  "99334726611ccf58a148b0814696bfa6fe08c1b2d027e946beccf5a74331c9aa",
		resultHex: "cabb04c6bd70ea7436c15b0ebfc8657d4ebb3516d28521c77fc51b0c6c39aac7",
	},
	"agent.web_search.config.update": {
		inputHex:  "8b1905bad022142ac879f8c8812d0fc0a0d3ea264f89edabd5caa617ae11defa",
		resultHex: "cabb04c6bd70ea7436c15b0ebfc8657d4ebb3516d28521c77fc51b0c6c39aac7",
	},
	"agent.web_search.test": {
		inputHex:  "99334726611ccf58a148b0814696bfa6fe08c1b2d027e946beccf5a74331c9aa",
		resultHex: "91ecbe4507c45e14f794bdd8569a8b9681d077f977485874592183dc7f5faccb",
	},
	"agent.text_tools.config.get": {
		inputHex:  "99334726611ccf58a148b0814696bfa6fe08c1b2d027e946beccf5a74331c9aa",
		resultHex: "ce1c828fed02c65c9ba92123d5e88f8087acec7ca3007c6fb57e6a1aa34eef56",
	},
	"agent.text_tools.config.update": {
		inputHex:  "27e28f1ad68d20dc25e63264a830391adcd2fb9b24203fce8b9e311022f87e1e",
		resultHex: "ce1c828fed02c65c9ba92123d5e88f8087acec7ca3007c6fb57e6a1aa34eef56",
	},
	"agent.text_tools.execute": {
		inputHex:  "59b37979b65e9e0b1bdc57bae5950f0d53533b10060741be7fc6a36529beb9bf",
		resultHex: "fa162b3374031e87711fa47067322839256115b2818d73506e8d99a288c9a316",
	},
	"agent.image_tools.upload.begin": {
		inputHex:  "2797dbc61371d343647866a9116151065eb633838f917d584c6394f237ea2b5a",
		resultHex: "5c1795955a5b5cd63c390c11fa24654389a9c42bea108584831eb7b0fa4f7bc7",
	},
	"agent.image_tools.upload.append": {
		inputHex:  "f3c65322ece7aca25c93e12046b4919aa6b942e5e252b3802ba86184898e013e",
		resultHex: "5c1795955a5b5cd63c390c11fa24654389a9c42bea108584831eb7b0fa4f7bc7",
	},
	"agent.image_tools.upload.commit": {
		inputHex:  "1f5a339393a28aaa60a08d4ba3d8e43d4320eef387d59f3524a4fb598320c875",
		resultHex: "2012c595a14cb1a5630822bbf4a033278d10dafccc97254c61ab7375ae21ce11",
	},
	"agent.image_tools.extract_text": {
		inputHex:  "475db24a1d6efb6ddeceeeb5f3bba9214bfa745b7046d65c61e7bb62fc2e7c8f",
		resultHex: "a9ae62acacbb5f5417461e9d46dde99eff836ccbabdd8e71c3d7b356a7ef829e",
	},
	"agent.image_tools.translate_text": {
		inputHex:  "695285a75e6b3c2d0706d01b50e7b3d57b072599efe362a48cb192ec44735a8d",
		resultHex: "b17197b1c57c81abf31fbb19a815d47f2af52d25236d999fb13d5553a2c34599",
	},
	"agent.core.model_profiles.sync": {
		inputHex:  "25374faf14f544c968d39368e2ba28394f7e2e04c51188a1cba37f1a0eec8b84",
		resultHex: "627867fcd8ef94e4425c8756c0b9ea76bc999626736d5656adf2ca6c68e3bb59",
	},
	"agent.core.model_profiles.list": {
		inputHex:  "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
		resultHex: "327e27f75fcac55ef777b6bf895df90cff1f3c7e3edd3f386fceeeceeb568e85",
	},
	"agent.core.model_profiles.get": {
		inputHex:  "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.core.model_profiles.delete": {
		inputHex:  "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.model_profiles.sync": {
		inputHex:  "25374faf14f544c968d39368e2ba28394f7e2e04c51188a1cba37f1a0eec8b84",
		resultHex: "627867fcd8ef94e4425c8756c0b9ea76bc999626736d5656adf2ca6c68e3bb59",
	},
	"agent.model_profiles.list": {
		inputHex:  "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
		resultHex: "327e27f75fcac55ef777b6bf895df90cff1f3c7e3edd3f386fceeeceeb568e85",
	},
	"agent.model_profiles.get": {
		inputHex:  "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.model_profiles.test": {
		inputHex:  "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.model_profiles.delete": {
		inputHex:  "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.knowledge.config.get": {
		inputHex:  "51da7ceec6376f5f1caac4b422cfa832718adde8bb2de8fda7940d2aff2b043f",
		resultHex: "87c332e9185a0436d6488041bbfc11cd66c9f40e345af02d9f97a76676cd65ae",
	},
	"agent.knowledge.config.update": {
		inputHex:  "c9d87f07b7dc15e181a272bfacceecfa82e331e516eff88358dd32c64661d97b",
		resultHex: "87c332e9185a0436d6488041bbfc11cd66c9f40e345af02d9f97a76676cd65ae",
	},
	"agent.knowledge.sources.list": {
		inputHex:  "b0418cb4b54dc5e1f787929dd8ba9c6fdd7d80d665779879e7da2a5a905aa08d",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.knowledge.sources.delete": {
		inputHex:  "122c4ab26b5340e5526f3433bae033acb51f5852bb81940a5d0d987328475c3a",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.knowledge.upload.start": {
		inputHex:  "d389f473339cc4f1a94221a28f5d5f39e1a21d9be6a46271d3e2b38c5c499839",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.knowledge.upload.chunk": {
		inputHex:  "edc7fded7efd20a9ad6c2cc66cec77889415a4709807d17cb0b9cd1cd3d648c4",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.knowledge.upload.finish": {
		inputHex:  "5c3ca14110e4195b443f226af01c2610b5595d612e198b1996b3341889c4e353",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.knowledge.memory.create": {
		inputHex:  "c04cf443c490416332723c300b6044f976bdd023f797a135a22b4ee47b6af216",
		resultHex: "57ed8c9f5f52f0eaf3e3692c17c567f1a7ddb3f1c69faa8163d707735da3299e",
	},
	"agent.knowledge.memories.list": {
		inputHex:  "0d8115d6c255081822e7cd5c17405596b480bc42ceb6623e7a5c793865d87be2",
		resultHex: "9651cc57500bee6ee243fe6eb27950eb194a9fe5996af90c7a3090d825c98085",
	},
	"agent.knowledge.memories.update": {
		inputHex:  "09d90a8d9865b2e768ab6ff43a35a009324b4abedb9fe305a851cb838ffcf117",
		resultHex: "5ff713d2c2628dcb1742f23725d3ee50347b578f58675b41c07f269e52793913",
	},
	"agent.knowledge.memories.delete": {
		inputHex:  "94749fd60006d5ac350fecd3f2f34fc85bc5bf894afa9acc209b8848484bee87",
		resultHex: "5ff713d2c2628dcb1742f23725d3ee50347b578f58675b41c07f269e52793913",
	},
	"agent.knowledge.search": {
		inputHex:  "50578c159dc6f9691d3c9e36a04f8216f1ff4d53414e35ac67cdbe8a43ebf62a",
		resultHex: "3bff0a96cb6f09421ee1a5ea243b8801a0b61fa4b1f8e01cdf98653acfd99761",
	},
	"agent.knowledge.status": {
		inputHex:  "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
		resultHex: "01c2cda93a058ad6e133692089afb4b81b9e5800d18003f6cd2f3e62f8efa4a3",
	},
	"agent.skills.list": {
		inputHex:  "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.skills.install": {
		inputHex:  "9acc5baf60e119e1870f8521f9a5dc7686bd7e7cc3f86a88db232efc095b3296",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.skills.enable": {
		inputHex:  "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.skills.disable": {
		inputHex:  "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.skills.uninstall": {
		inputHex:  "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.skills.registry.search": {
		inputHex:  "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.mcp.servers.list": {
		inputHex:  "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.mcp.servers.install": {
		inputHex:  "d298a2044537c6fe426468db657e114784169bc3404008073ac37e6b29a62d83",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.mcp.servers.enable": {
		inputHex:  "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.mcp.servers.disable": {
		inputHex:  "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.mcp.servers.uninstall": {
		inputHex:  "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.mcp.registry.search": {
		inputHex:  "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.core.mcp.discover": {
		inputHex:  "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.core.mcp.get": {
		inputHex:  "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.core.mcp.list": {
		inputHex:  "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.core.mcp.inspect": {
		inputHex:  "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.core.mcp.install": {
		inputHex:  "d298a2044537c6fe426468db657e114784169bc3404008073ac37e6b29a62d83",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.core.mcp.update": {
		inputHex:  "38e0d16c5ba8fb0ee20323ddb997840cdbaae6aa9b16dfe6e20f992ca2ee735c",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.core.mcp.remove": {
		inputHex:  "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.core.mcp.list_tools": {
		inputHex:  "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.core.mcp.execute": {
		inputHex:  "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.core.skills.discover": {
		inputHex:  "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.core.skills.get": {
		inputHex:  "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.core.skills.list": {
		inputHex:  "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.core.skills.inspect": {
		inputHex:  "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.core.skills.install": {
		inputHex:  "9acc5baf60e119e1870f8521f9a5dc7686bd7e7cc3f86a88db232efc095b3296",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.core.skills.update": {
		inputHex:  "21a4aae561dcc8d6078c6f1637402e62711f7170b1da205e5ecc246b9ac8ae20",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.core.skills.remove": {
		inputHex:  "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.core.skills.execute": {
		inputHex:  "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.core.tasks.get": {
		inputHex:  "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.core.tasks.list": {
		inputHex:  "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.core.tasks.cancel": {
		inputHex:  "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.core.tasks.retry": {
		inputHex:  "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.core.tasks.events": {
		inputHex:  "0f20333679950dfb5469f2e55f83ef7ab14926c63372709271c402c7fbffd6a6",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.core.schedules.create": {
		inputHex:  "18091849c7221af30b25417c427eb76a024030347c7f4cae2e583941f03dd7db",
		resultHex: "b83f7386064825b5fea88825f74fa523e6183ef1e4a6b6798e7fe2ba106d5916",
	},
	"agent.core.schedules.get": {
		inputHex:  "20b67d6d5b3e896a7693aa2a7ea9612e60fea362875bc58b8e1289b165dec142",
		resultHex: "b83f7386064825b5fea88825f74fa523e6183ef1e4a6b6798e7fe2ba106d5916",
	},
	"agent.core.schedules.list": {
		inputHex:  "bb2f93e43dd9903fb383aa36e6948959af0585995f50debfd63ca793bf9c92b6",
		resultHex: "ac874bcbc2e89c6d9ffa64e0bcfb0eb1d2280417b10cfef5b318fb3f39e55b56",
	},
	"agent.core.schedules.update": {
		inputHex:  "0770d5814e399ca496440fb85d1a53e3544bdd8eac7d80b1f0b63c0d0b151bad",
		resultHex: "b83f7386064825b5fea88825f74fa523e6183ef1e4a6b6798e7fe2ba106d5916",
	},
	"agent.core.schedules.pause": {
		inputHex:  "7fc2bc9cfa33d71ae2d24e23afc70f94078edfbb6199a1db5f66994ab9d1c8cf",
		resultHex: "b83f7386064825b5fea88825f74fa523e6183ef1e4a6b6798e7fe2ba106d5916",
	},
	"agent.core.schedules.resume": {
		inputHex:  "7fc2bc9cfa33d71ae2d24e23afc70f94078edfbb6199a1db5f66994ab9d1c8cf",
		resultHex: "b83f7386064825b5fea88825f74fa523e6183ef1e4a6b6798e7fe2ba106d5916",
	},
	"agent.core.schedules.trigger": {
		inputHex:  "2e61f36e3bf5b2e76a20783a6225bbc42f9a04a2d55873efc8e3ccbd3f00d2ec",
		resultHex: "fdce1e4467f447010e3598d7ea8697ae1ae6abff58b201eba0e01d49e0849788",
	},
	"agent.core.schedules.delete": {
		inputHex:  "7fc2bc9cfa33d71ae2d24e23afc70f94078edfbb6199a1db5f66994ab9d1c8cf",
		resultHex: "87be66ec1c340a83fc6036b5e7c25ccbbd7e379f9725f76a980f57af609ad5cf",
	},
	"agent.core.confirmations.get": {
		inputHex:  "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.core.confirmations.list": {
		inputHex:  "cf513c82f6b4db00bc291259202f16d001983ca4b17159dce1635d072d8bd244",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.core.confirmations.confirm": {
		inputHex:  "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.core.confirmations.reject": {
		inputHex:  "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.core.confirmations.acknowledge_extension_execution_uncertain": {
		inputHex:  "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.core.aws.credentials.create": {
		inputHex:  "a9d2bb552e36122f19a0ecea71822ff460de4369709fe8f9ae5f38284ca2c8a8",
		resultHex: "0954ec02d95a3acf6afab37381521763e20e4a197bd6ddd0e4008322c5b7ce42",
	},
	"agent.core.aws.credentials.update": {
		inputHex:  "a8c650b1c8ce20e1a557dd6637717e9dacbc55caae755d10c9ad3f0ce28c62d3",
		resultHex: "0954ec02d95a3acf6afab37381521763e20e4a197bd6ddd0e4008322c5b7ce42",
	},
	"agent.core.aws.credentials.delete": {
		inputHex:  "586b90539d52e704123fdb58128f59f685049224f1c6a7b71527729cc05a7c9b",
		resultHex: "9730c2d9e4946803d259afd4755a9b991d3003d057ed48314f3e5002e69a2112",
	},
	"agent.core.aws.credentials.list": {
		inputHex:  "1f723750dd875e188ccfcd9ce846dc79e0e0e6e6e73225e505e7e0bfbc4ffe7b",
		resultHex: "5b4b96856d4905ab16617e86afd4e5f558e21929488edee989ba701932b3938c",
	},
	"agent.core.aws.credentials.test": {
		inputHex:  "cf4245b73f19ef99b8481d487e369317fda8e9997e57db85aff5a6848847684a",
		resultHex: "fd1394709469435c496de2ce805250f4229211f550ae82bd7d2d647682a5aa6c",
	},
	"agent.voice.session.create": {
		inputHex:  "2a9d9b82b94ed22ca8771c014f568efb089006bda98db1402e605ca3379bbe8d",
		resultHex: "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
	},
	"agent.voice.session.start": {
		inputHex:  "a4ad03b0dba86d416d70e7bfa889285f6448e709e7cb0348e5cc1dbfd02eedf5",
		resultHex: "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
	},
	"agent.voice.session.transcript": {
		inputHex:  "b9169e9ee1f3a9a08b0f81686ec5cc9294009d5c52c1cbf13f5bac365419635c",
		resultHex: "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
	},
	"agent.voice.session.interrupt": {
		inputHex:  "a4ad03b0dba86d416d70e7bfa889285f6448e709e7cb0348e5cc1dbfd02eedf5",
		resultHex: "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
	},
	"agent.voice.session.end": {
		inputHex:  "a4ad03b0dba86d416d70e7bfa889285f6448e709e7cb0348e5cc1dbfd02eedf5",
		resultHex: "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
	},
	"agent.voice.session.stream": {
		inputHex:  "953caef4141594b512538480acb285bf7a366254f6d9ae56fd6202181b78951f",
		resultHex: "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
	},
	"agent.execution.v2.projects.analyze": {
		inputHex:  "a2836c5767771a6d0c2f1ca2f51fa1562c327fa6c63b3b61c6f6bcb32beed5ed",
		resultHex: "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
	},
	"agent.execution.v2.analyses.get": {
		inputHex:  "19dbfd911a4a7a0e7b8528dd9663d0854f0f660211a83ee49a5c65418c584540",
		resultHex: "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
	},
	"agent.execution.v2.targets.list": {
		inputHex:  "d8b031142e91a514ecb1652dbaef1ae99756493d92c79636ac46e81645ca8319",
		resultHex: "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
	},
	"agent.execution.v2.targets.get": {
		inputHex:  "de5c518d323ccdb6492ede0a3cba6fd547616d8f44be77c72620de4d1c67b1a4",
		resultHex: "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
	},
	"agent.execution.v2.targets.import": {
		inputHex:  "c09d9c2952eb824fbb91b862c9faf9b77b7b337da6c5ae1562befebecb865c87",
		resultHex: "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
	},
	"agent.execution.v2.targets.reserve": {
		inputHex:  "40ca0744b8a00cec78b691d5762c5ef97b3c3fbaca22a4e00d34375714eee65c",
		resultHex: "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
	},
	"agent.execution.v2.targets.observe": {
		inputHex:  "58622f1e9f3af1076a2e411bd4303136227d9ff726229babfae6e4f3f938112f",
		resultHex: "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
	},
	"agent.execution.v2.plans.create": {
		inputHex:  "5aa1fe5bd33525f43b0909fa69917f6568a52799a6daa0422ff6bc03c68a2565",
		resultHex: "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
	},
	"agent.execution.v2.plans.revise": {
		inputHex:  "03953da6efb3578f8c94aa9391f080b8ab0aa0e522d2bdc7e308010dbeb33051",
		resultHex: "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
	},
	"agent.execution.v2.plans.get": {
		inputHex:  "16a2421bc9fe203599cd6ac7041af4418c1eb91ee763e67f60cc026a187275d2",
		resultHex: "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
	},
	"agent.execution.v2.plans.list": {
		inputHex:  "e51cedcd24b3fd93987a55942194f24f3e9a6d567a6d69467152ae63e1e6517c",
		resultHex: "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
	},
	"agent.execution.v2.deployments.list": {
		inputHex:  "240ddd168d1005ce6be5d887188a13cbecce17389c29a6436f576122effbd5b3",
		resultHex: "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
	},
	"agent.execution.v2.deployments.get": {
		inputHex:  "adcbb60d1a900ac6f5297dda341c19653c5d75c6385307bc007dd10e96df9cd7",
		resultHex: "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
	},
	"agent.execution.v2.deployments.events": {
		inputHex:  "b68941374a73015596e238a8396da08577541ac3d132d6d30fb48fe2ba6add49",
		resultHex: "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
	},
	"agent.execution.v2.runs.create": {
		inputHex:  "40122ead164aa76be951f06dd3a9a9230592d229d018002716d7280f380f143f",
		resultHex: "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
	},
	"agent.execution.v2.runs.get": {
		inputHex:  "731ab9f17fc407152f12821c1d12fd2f04e7ebb27217da3e46b0caaa7be310b7",
		resultHex: "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
	},
	"agent.execution.v2.runs.list": {
		inputHex:  "ca52b0972160b4d4f59cc89e79c7cfdc475ce345dc38af338942c02c0efa63c1",
		resultHex: "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
	},
	"agent.execution.v2.runs.cancel": {
		inputHex:  "8d9994a07f21125d3b6e1fd314244ff8b8bab060fa4b7a9adb7c22138cb30ee2",
		resultHex: "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
	},
	"agent.execution.v2.runs.retry": {
		inputHex:  "7e180c570ec547e4984ba496e33c79014ebf52395a0ed4bd5ce415a8e807b030",
		resultHex: "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
	},
	"agent.execution.v2.runs.events": {
		inputHex:  "b449e03d00d680a77a0119ac7120ca862409f3c4eaf8e53ebf723585af1ca637",
		resultHex: "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
	},
	"agent.execution.v2.artifacts.get": {
		inputHex:  "203e2e90b8b8e13a18f62b919c4ab4227625fffd8e78ec23e7121d1f444297a5",
		resultHex: "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
	},
	"agent.execution.v2.artifacts.download": {
		inputHex:  "1f89699ab07b14d135619ee5f6b2ffd0d8d0821fb8f1ba236662814c0586706c",
		resultHex: "6ea5feead715aa50feeff464e6da618564f9b6e422025c94743faf173478689d",
	},
	"agent.execution.v2.service_bindings.list": {
		inputHex:  "240ddd168d1005ce6be5d887188a13cbecce17389c29a6436f576122effbd5b3",
		resultHex: "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
	},
	"agent.execution.v2.service_bindings.get": {
		inputHex:  "9dd3d8f9573a1c13c3908de2feb8d9eab363f63a5bf5254b41c6ea1ae756469b",
		resultHex: "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
	},
	"agent.execution.v2.service_bindings.invoke": {
		inputHex:  "96c7957c6ed17de2cdb662dca89cf9b9c3fa1eb895b1a7f3ba7177976a4850d1",
		resultHex: "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
	},
	"agent.execution.v2.secrets.create": {
		inputHex:  "9dc7ae110dfbe03b7277ee6d1d1c3a8cbd6ec6793e04e741eefa3309ba0dcd12",
		resultHex: "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
	},
	"agent.execution.v2.secrets.get": {
		inputHex:  "94610e4ad1e23013ecaf4184d4d62ea5af5ad019515c14feff1b0cab1df4c1b7",
		resultHex: "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
	},
	"agent.execution.v2.secrets.list": {
		inputHex:  "d8b031142e91a514ecb1652dbaef1ae99756493d92c79636ac46e81645ca8319",
		resultHex: "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
	},
	"agent.execution.v2.secrets.revoke": {
		inputHex:  "420f5b609052d4ded3ed4d89d45344e056e35bac64f61a0b4eee30e79a40482b",
		resultHex: "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
	},
	"agent.contacts.list": {
		inputHex:  "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.contacts.search": {
		inputHex:  "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.rooms.search": {
		inputHex:  "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.messages.list": {
		inputHex:  "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.messages.send": {
		inputHex:  "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.room_members.list": {
		inputHex:  "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.channel_posts.list": {
		inputHex:  "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.channel_comments.list": {
		inputHex:  "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
	"agent.channel_comments.create": {
		inputHex:  "ac79d5f21b24bc32782380d222adfb19224f8c68945623ee78496c56c56d870e",
		resultHex: "a2c799262a3ce3c19ef5cdd983bf3d12b43ab3c426227091b909dcb7054738c0",
	},
}

// NewCatalogRequirement binds a public action to the current Agent schema
// identities when one is part of the readiness proof. Unknown/optional
// actions still receive the normal self-consistency check in ValidateCatalog.
func NewCatalogRequirement(action string) CatalogRequirement {
	requirement := CatalogRequirement{Action: strings.TrimSpace(action)}
	digest, ok := expectedCatalogSchemaDigests[requirement.Action]
	if !ok {
		// Every action that can reach the live gateway lookup is a frozen
		// contract, even if a newly-added binding has not yet been added to the
		// generated digest table. Keep readiness fail-closed for that case while
		// preserving self-consistency validation for explicit optional extras.
		if _, bound := actionBindings[requirement.Action]; bound {
			requirement.RequireSchemaPin = true
		}
		return requirement
	}
	requirement.RequireSchemaPin = true
	requirement.InputSchemaDigest = decodeCatalogSchemaDigest(digest.inputHex)
	requirement.ResultSchemaDigest = decodeCatalogSchemaDigest(digest.resultHex)
	if digest.eventHex != "" {
		requirement.RequireEventSchemaPin = true
		requirement.EventSchemaDigest = decodeCatalogSchemaDigest(digest.eventHex)
	}
	return requirement
}

// catalogRequirementForLookup is stricter than NewCatalogRequirement's
// optional-extension behavior. A live action binding must have both exact
// schema pins before any catalog is trusted for request signing or dispatch.
func catalogRequirementForLookup(action string) (CatalogRequirement, error) {
	requirement := NewCatalogRequirement(action)
	if _, bound := actionBindings[strings.TrimSpace(action)]; bound &&
		(len(requirement.InputSchemaDigest) != sha256.Size || len(requirement.ResultSchemaDigest) != sha256.Size ||
			requirement.RequireEventSchemaPin && len(requirement.EventSchemaDigest) != sha256.Size) {
		return requirement, fmt.Errorf("%w: action %q has no pinned schema identity", ErrCatalogInvalid, strings.TrimSpace(action))
	}
	return requirement, nil
}

func decodeCatalogSchemaDigest(encoded string) []byte {
	digest, err := hex.DecodeString(encoded)
	if err != nil {
		return nil
	}
	return digest
}
