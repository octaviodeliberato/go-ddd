package services

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/sklinkert/go-ddd/internal/application/command"
	"github.com/sklinkert/go-ddd/internal/application/interfaces"
	"github.com/sklinkert/go-ddd/internal/application/mapper"
	"github.com/sklinkert/go-ddd/internal/application/query"
	"github.com/sklinkert/go-ddd/internal/domain/entities"
	"github.com/sklinkert/go-ddd/internal/domain/repositories"
)

// ProductService implements the ProductService interface defined in internal/application/interfaces.
//
// METHOD IMPLEMENTATION EXPLANATION:
//
// The ProductService methods (CreateProduct, UpdateProduct, DeleteProduct, FindAllProducts, FindProductById)
// are implemented DIRECTLY in this struct. They are NOT inherited or delegated from elsewhere.
//
// Here's how the architecture works:
//
// 1. SERVICE INTERFACE (internal/application/interfaces/product_service.go):
//   - Defines the contract that any ProductService implementation must fulfill
//   - Lists method signatures: CreateProduct, UpdateProduct, DeleteProduct, FindAllProducts, FindProductById
//
// 2. SERVICE IMPLEMENTATION (this file):
//   - This ProductService struct implements all methods defined in the interface
//   - Each method contains the business logic and orchestration code
//   - Methods are written directly in this file (see methods below)
//
// 3. REPOSITORY PATTERN:
//   - ProductService depends on repository INTERFACES (not concrete implementations)
//   - Repository interfaces are defined in internal/domain/repositories/
//   - Concrete implementations are in internal/infrastructure/db/postgres/
//   - Repositories are injected via the constructor (NewProductService)
//
// 4. DEPENDENCY INJECTION:
//   - The constructor NewProductService receives repository implementations
//   - These repositories are stored as struct fields
//   - Service methods use these repositories to perform data operations
//   - Example: s.productRepository.Create(...), s.sellerRepository.FindById(...)
//
// 5. LAYERED ARCHITECTURE (DDD/Onion Architecture):
//   - Domain Layer: Defines entities and repository interfaces
//   - Application Layer: This service orchestrates business operations
//   - Infrastructure Layer: Provides concrete repository implementations (e.g., PostgreSQL)
//   - The application layer knows about domain but not about infrastructure details
//
// In summary: The service methods are implemented HERE in this file. They use injected
// repositories to interact with data storage, following the Dependency Inversion Principle.
type ProductService struct {
	productRepository repositories.ProductRepository
	sellerRepository  repositories.SellerRepository
	idempotencyRepo   repositories.IdempotencyRepository
}

// NewProductService is the constructor that creates a new ProductService instance.
//
// DEPENDENCY INJECTION EXPLANATION:
// This constructor follows the Dependency Injection pattern. It receives repository
// implementations as parameters and stores them in the ProductService struct.
//
// Parameters:
//   - productRepository: Implements repositories.ProductRepository interface
//   - sellerRepository: Implements repositories.SellerRepository interface
//   - idempotencyRepo: Implements repositories.IdempotencyRepository interface
//
// The actual implementations (e.g., SqlcProductRepository from infrastructure layer)
// are passed in by the caller (typically in cmd/marketplace/main.go during application startup).
//
// This pattern allows:
//   - Easy testing with mock repositories
//   - Decoupling from specific database implementations
//   - Flexibility to change persistence mechanisms without changing service code
//
// Returns interfaces.ProductService which is implemented by *ProductService
func NewProductService(
	productRepository repositories.ProductRepository,
	sellerRepository repositories.SellerRepository,
	idempotencyRepo repositories.IdempotencyRepository,
) interfaces.ProductService {
	return &ProductService{
		productRepository: productRepository,
		sellerRepository:  sellerRepository,
		idempotencyRepo:   idempotencyRepo,
	}
}

// CreateProduct implements the ProductService interface method for creating a new product.
//
// METHOD IMPLEMENTATION:
// This method is implemented directly here in the ProductService struct. The implementation:
//
// 1. Handles idempotency to prevent duplicate operations
// 2. Validates that the seller exists using sellerRepository
// 3. Creates and validates a new Product entity (domain layer responsibility)
// 4. Persists the product using productRepository.Create()
// 5. Returns the result wrapped in a command result object
//
// REPOSITORY USAGE:
// - s.idempotencyRepo.FindByKey() / Create() - for idempotency checking
// - s.sellerRepository.FindById() - to validate seller exists
// - s.productRepository.Create() - to persist the new product
//
// The repositories handle all data access, while this service method orchestrates
// the business flow and enforces business rules.
func (s *ProductService) CreateProduct(productCommand *command.CreateProductCommand) (*command.CreateProductCommandResult, error) {
	ctx := context.Background()

	// Check idempotency key
	if productCommand.IdempotencyKey != "" {
		existingRecord, err := s.idempotencyRepo.FindByKey(ctx, productCommand.IdempotencyKey)
		if err != nil {
			return nil, err
		}

		if existingRecord != nil {
			// Return cached response
			var result command.CreateProductCommandResult
			if err := json.Unmarshal([]byte(existingRecord.Response), &result); err != nil {
				return nil, err
			}
			return &result, nil
		}
	}

	// Create idempotency record
	var idempotencyRecord *entities.IdempotencyRecord
	if productCommand.IdempotencyKey != "" {
		requestJSON, _ := json.Marshal(productCommand)
		idempotencyRecord = entities.NewIdempotencyRecord(productCommand.IdempotencyKey, string(requestJSON))
	}

	storedSeller, err := s.sellerRepository.FindById(productCommand.SellerId)
	if err != nil {
		return nil, err
	}

	if storedSeller == nil {
		return nil, errors.New("seller not found")
	}

	validatedSeller, err := entities.NewValidatedSeller(storedSeller)
	if err != nil {
		return nil, err
	}

	var newProduct = entities.NewProduct(
		productCommand.Name,
		productCommand.Price,
		*validatedSeller,
	)

	validatedProduct, err := entities.NewValidatedProduct(newProduct)
	if err != nil {
		return nil, err
	}

	_, err = s.productRepository.Create(validatedProduct)
	if err != nil {
		return nil, err
	}

	result := command.CreateProductCommandResult{
		Result: mapper.NewProductResultFromValidatedEntity(validatedProduct),
	}

	// Store response in idempotency record
	if idempotencyRecord != nil {
		responseJSON, _ := json.Marshal(result)
		idempotencyRecord.SetResponse(string(responseJSON), 200)
		_, err = s.idempotencyRepo.Create(ctx, idempotencyRecord)
		if err != nil {
			// Log error but don't fail the operation
			// In production, you might want to handle this differently
		}
	}

	return &result, nil
}

// FindAllProducts implements the ProductService interface method for retrieving all products.
//
// METHOD IMPLEMENTATION:
// This is a QUERY method (read operation) that:
//
// 1. Calls s.productRepository.FindAll() to retrieve all products from storage
// 2. Maps domain entities to query result objects using the mapper
// 3. Returns the query result
//
// REPOSITORY USAGE:
// - s.productRepository.FindAll() - retrieves all products from the database
//
// Note: This method has NO side effects and doesn't modify any state (CQRS Query pattern).
func (s *ProductService) FindAllProducts() (*query.GetAllProductsQueryResult, error) {
	storedProducts, err := s.productRepository.FindAll()
	if err != nil {
		return nil, err
	}

	var queryListResult query.GetAllProductsQueryResult
	for _, product := range storedProducts {
		queryListResult.Result = append(queryListResult.Result, mapper.NewProductResultFromEntity(product))
	}

	return &queryListResult, nil
}

func (s *ProductService) FindProductById(productQuery *query.GetProductByIdQuery) (*query.GetProductByIdQueryResult, error) {
	storedProduct, err := s.productRepository.FindById(productQuery.Id)
	if err != nil {
		return nil, err
	}

	var queryResult query.GetProductByIdQueryResult
	queryResult.Result = mapper.NewProductResultFromEntity(storedProduct)

	return &queryResult, nil
}

func (s *ProductService) UpdateProduct(productCommand *command.UpdateProductCommand) (*command.UpdateProductCommandResult, error) {
	ctx := context.Background()

	// Check idempotency key
	if productCommand.IdempotencyKey != "" {
		existingRecord, err := s.idempotencyRepo.FindByKey(ctx, productCommand.IdempotencyKey)
		if err != nil {
			return nil, err
		}

		if existingRecord != nil {
			// Return cached response
			var result command.UpdateProductCommandResult
			if err := json.Unmarshal([]byte(existingRecord.Response), &result); err != nil {
				return nil, err
			}
			return &result, nil
		}
	}

	// Create idempotency record
	var idempotencyRecord *entities.IdempotencyRecord
	if productCommand.IdempotencyKey != "" {
		requestJSON, _ := json.Marshal(productCommand)
		idempotencyRecord = entities.NewIdempotencyRecord(productCommand.IdempotencyKey, string(requestJSON))
	}

	// Find existing product
	existingProduct, err := s.productRepository.FindById(productCommand.Id)
	if err != nil {
		return nil, err
	}

	if existingProduct == nil {
		return nil, errors.New("product not found")
	}

	// Find seller if different
	if productCommand.SellerId != existingProduct.Seller.Id {
		storedSeller, err := s.sellerRepository.FindById(productCommand.SellerId)
		if err != nil {
			return nil, err
		}

		if storedSeller == nil {
			return nil, errors.New("seller not found")
		}

		validatedSeller, err := entities.NewValidatedSeller(storedSeller)
		if err != nil {
			return nil, err
		}
		existingProduct.Seller = validatedSeller.Seller
	}

	// Update product fields
	if err := existingProduct.UpdateName(productCommand.Name); err != nil {
		return nil, err
	}

	if err := existingProduct.UpdatePrice(productCommand.Price); err != nil {
		return nil, err
	}

	validatedProduct, err := entities.NewValidatedProduct(existingProduct)
	if err != nil {
		return nil, err
	}

	_, err = s.productRepository.Update(validatedProduct)
	if err != nil {
		return nil, err
	}

	result := command.UpdateProductCommandResult{
		Result: mapper.NewProductResultFromValidatedEntity(validatedProduct),
	}

	// Store response in idempotency record
	if idempotencyRecord != nil {
		responseJSON, _ := json.Marshal(result)
		idempotencyRecord.SetResponse(string(responseJSON), 200)
		_, err = s.idempotencyRepo.Create(ctx, idempotencyRecord)
		if err != nil {
			// Log error but don't fail the operation
			// In production, you might want to handle this differently
		}
	}

	return &result, nil
}
func (s *ProductService) DeleteProduct(productCommand *command.DeleteProductCommand) (*command.DeleteProductCommandResult, error) {
	ctx := context.Background()

	// Check idempotency key
	if productCommand.IdempotencyKey != "" {
		existingRecord, err := s.idempotencyRepo.FindByKey(ctx, productCommand.IdempotencyKey)
		if err != nil {
			return nil, err
		}

		if existingRecord != nil {
			// Return cached response
			var result command.DeleteProductCommandResult
			if err := json.Unmarshal([]byte(existingRecord.Response), &result); err != nil {
				return nil, err
			}
			return &result, nil
		}
	}

	// Create idempotency record
	var idempotencyRecord *entities.IdempotencyRecord
	if productCommand.IdempotencyKey != "" {
		requestJSON, _ := json.Marshal(productCommand)
		idempotencyRecord = entities.NewIdempotencyRecord(productCommand.IdempotencyKey, string(requestJSON))
	}

	// Check if product exists
	existingProduct, err := s.productRepository.FindById(productCommand.Id)
	if err != nil {
		return nil, err
	}

	if existingProduct == nil {
		return nil, errors.New("product not found")
	}

	// Delete product
	err = s.productRepository.Delete(productCommand.Id)
	if err != nil {
		return nil, err
	}

	result := command.DeleteProductCommandResult{
		Success: true,
	}

	// Store response in idempotency record
	if idempotencyRecord != nil {
		responseJSON, _ := json.Marshal(result)
		idempotencyRecord.SetResponse(string(responseJSON), 200)
		_, err = s.idempotencyRepo.Create(ctx, idempotencyRecord)
		if err != nil {
			// Log error but don't fail the operation
			// In production, you might want to handle this differently
		}
	}

	return &result, nil
}
